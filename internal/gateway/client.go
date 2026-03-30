package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"time"
)

// ForwardRequest is the payload OpenClaw's /hooks/agent endpoint expects.
type ForwardRequest struct {
	Message    string `json:"message"`
	Name       string `json:"name"`
	SessionKey string `json:"sessionKey,omitempty"`
	Deliver    bool   `json:"deliver"`
	// Internal fields — not serialised.
	RequestID  string    `json:"-"`
	ReceivedAt time.Time `json:"-"`
}

// Config holds the OpenClaw gateway coordinates and retry policy.
type Config struct {
	// BaseURL is the OpenClaw API root, e.g. "https://api.openclaw.io/v1"
	BaseURL string

	// APIKey is the bearer token for OpenClaw authentication.
	APIKey string

	// MaxRetries is the number of additional attempts after the first failure.
	MaxRetries int

	// InitialBackoff is the wait time before the first retry.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential growth to avoid unreasonably long waits.
	MaxBackoff time.Duration

	// HTTPTimeout is the per-attempt deadline for non-streaming requests.
	HTTPTimeout time.Duration
}

// DefaultConfig returns a sensible baseline for production use.
func DefaultConfig(baseURL, apiKey string) Config {
	return Config{
		BaseURL:        baseURL,
		APIKey:         apiKey,
		MaxRetries:     4,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		HTTPTimeout:    15 * time.Second,
	}
}

// Client is the OpenClaw gateway HTTP client.
// All methods are safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
}

// New constructs a Client. The underlying http.Client is shared across all
// requests — connection pooling is handled automatically by http.Transport.
func New(cfg Config) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: cfg.HTTPTimeout,
		},
	}
}

func (c *Client) Forward(ctx context.Context, req ForwardRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal forward request: %w", err)
	}

	return c.doWithRetry(ctx, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			c.cfg.BaseURL+"/hooks/agent",
			bytes.NewReader(body),
		)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

		return c.http.Do(httpReq)
	})
}

// StreamFile pipes a file from srcURL (a Zoho file download URL) directly to
// the OpenClaw Workspace API without loading the file into memory.
// zohoToken is a valid Zoho OAuth access token used to authenticate the download.
func (c *Client) StreamFile(ctx context.Context, srcURL, filename, mimeType, zohoToken string) error {
	pr, pw := io.Pipe()

	go func() {
		defer pw.Close()

		dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
		if err != nil {
			pw.CloseWithError(fmt.Errorf("build zoho download request: %w", err))
			return
		}
		// Authenticate the download request with the caller-supplied Zoho token.
		if zohoToken != "" {
			dlReq.Header.Set("Authorization", "Zoho-oauthtoken "+zohoToken)
		}

		resp, err := c.http.Do(dlReq)
		if err != nil {
			pw.CloseWithError(fmt.Errorf("zoho download: %w", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			pw.CloseWithError(fmt.Errorf("zoho download status: %s", resp.Status))
			return
		}

		if _, err := io.Copy(pw, resp.Body); err != nil {
			pw.CloseWithError(fmt.Errorf("pipe copy: %w", err))
		}
	}()

	return c.doWithRetry(ctx, func() (*http.Response, error) {
		uploadReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			c.cfg.BaseURL+"/workspace/files",
			pr,
		)
		if err != nil {
			return nil, fmt.Errorf("build upload request: %w", err)
		}
		uploadReq.Header.Set("Content-Type", mimeType)
		uploadReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		uploadReq.Header.Set("X-Filename", filename)

		return c.http.Do(uploadReq)
	})
}

// doWithRetry executes fn and retries on retryable failures.
// It respects the parent context — a cancelled context aborts immediately
// without consuming remaining retry attempts.
func (c *Client) doWithRetry(ctx context.Context, fn func() (*http.Response, error)) error {
	var lastErr error

	for attempt := range c.cfg.MaxRetries + 1 {
		// Abort early if the caller's context is already done.
		if ctx.Err() != nil {
			return fmt.Errorf("context cancelled before attempt %d: %w", attempt, ctx.Err())
		}

		resp, err := fn()
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: %w", attempt, err)
			slog.Warn("gateway request failed, will retry",
				"attempt", attempt,
				"error", lastErr,
			)
		} else {
			resp.Body.Close()

			if !isRetryable(resp.StatusCode) {
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
				// 4xx (except 429) are permanent failures — do not retry.
				return fmt.Errorf("permanent failure: status %d", resp.StatusCode)
			}

			lastErr = fmt.Errorf("attempt %d: retryable status %d", attempt, resp.StatusCode)
			slog.Warn("gateway returned retryable status",
				"attempt", attempt,
				"status", resp.StatusCode,
			)
		}

		if attempt == c.cfg.MaxRetries {
			break
		}

		wait := backoffDuration(attempt, c.cfg.InitialBackoff, c.cfg.MaxBackoff)
		slog.Info("backing off before retry",
			"attempt", attempt,
			"wait_ms", wait.Milliseconds(),
		)

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during backoff: %w", ctx.Err())
		case <-time.After(wait):
		}
	}

	return fmt.Errorf("all %d attempts failed: %w", c.cfg.MaxRetries+1, lastErr)
}

// isRetryable returns true for status codes that warrant a retry.
// 429 and 5xx are transient by convention; everything else is permanent.
func isRetryable(code int) bool {
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

// backoffDuration computes the wait time for a given attempt using
// exponential backoff with full jitter to avoid thundering herd problems.
//
//	wait = random(0, min(maxBackoff, initialBackoff * 2^attempt))
func backoffDuration(attempt int, initial, max time.Duration) time.Duration {
	exp := math.Pow(2, float64(attempt))
	cap := time.Duration(float64(initial) * exp)
	if cap > max {
		cap = max
	}
	// Full jitter: random value in [0, cap].
	jittered := time.Duration(rand.Float64() * float64(cap))
	return jittered
}

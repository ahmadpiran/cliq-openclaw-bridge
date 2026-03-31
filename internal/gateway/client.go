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
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ForwardRequest struct {
	Message    string    `json:"message"`
	Name       string    `json:"name"`
	SessionKey string    `json:"sessionKey,omitempty"`
	RequestID  string    `json:"-"`
	ReceivedAt time.Time `json:"-"`
}

type Config struct {
	BaseURL        string
	APIKey         string
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	HTTPTimeout    time.Duration
}

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

type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.HTTPTimeout},
	}
}

// Forward sends a message to OpenClaw's /hooks/agent endpoint.
func (c *Client) Forward(ctx context.Context, req ForwardRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal forward request: %w", err)
	}

	slog.Debug("forwarding to openclaw",
		"endpoint", c.cfg.BaseURL+"/hooks/agent",
		"payload", string(body),
	)

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

// DownloadAndForward downloads a file from Zoho, saves it to the OpenClaw
// workspace, then sends the agent a message referencing the saved path.
// OpenClaw's native pdf tool handles text extraction and image rasterization.
func (c *Client) DownloadAndForward(
	ctx context.Context,
	srcURL, filename, mimeType, zohoToken, comment, sessionKey, workspaceDir string,
) error {
	// --- Download from Zoho ---
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	if zohoToken != "" {
		dlReq.Header.Set("Authorization", "Zoho-oauthtoken "+zohoToken)
	}

	resp, err := c.http.Do(dlReq)
	if err != nil {
		return fmt.Errorf("download file from zoho: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zoho download status: %s", resp.Status)
	}

	// --- Save to workspace ---
	uploadsDir := filepath.Join(workspaceDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		return fmt.Errorf("create uploads dir: %w", err)
	}

	// Sanitise filename to prevent path traversal.
	safeFilename := filepath.Base(filename)
	destPath := filepath.Join(uploadsDir, safeFilename)

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create workspace file: %w", err)
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(destPath)
		return fmt.Errorf("write workspace file: %w", err)
	}

	slog.Info("file saved to workspace",
		"filename", safeFilename,
		"bytes", written,
		"dest", destPath,
	)

	// --- Tell the agent to process it ---
	// Use the OpenClaw container's internal workspace path.
	agentPath := "~/workspace/uploads/" + safeFilename

	var message string
	if comment != "" {
		message = fmt.Sprintf("%s\n\n[File saved to workspace: %s | Type: %s]",
			comment, agentPath, mimeType)
	} else {
		message = fmt.Sprintf(
			"A file has been shared. Please process it: %s (type: %s)",
			agentPath, mimeType,
		)
	}

	return c.Forward(ctx, ForwardRequest{
		Message:    message,
		Name:       "Zoho Cliq",
		SessionKey: sessionKey,
	})
}

func (c *Client) doWithRetry(ctx context.Context, fn func() (*http.Response, error)) error {
	var lastErr error

	for attempt := range c.cfg.MaxRetries + 1 {
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

func isRetryable(code int) bool {
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

func backoffDuration(attempt int, initial, max time.Duration) time.Duration {
	exp := math.Pow(2, float64(attempt))
	cap := time.Duration(float64(initial) * exp)
	if cap > max {
		cap = max
	}
	return time.Duration(rand.Float64() * float64(cap))
}

// isTextMime reports whether the MIME type content can be safely inlined as text.
// Kept for reference — currently unused since we write all files to workspace.
func isTextMime(mime string) bool {
	textTypes := []string{
		"text/",
		"application/json",
		"application/xml",
		"application/javascript",
		"application/x-yaml",
		"application/yaml",
	}
	for _, t := range textTypes {
		if strings.HasPrefix(mime, t) {
			return true
		}
	}
	return false
}

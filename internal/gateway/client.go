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

// ForwardRequest is kept for the legacy hooks endpoint (used by forwardRaw).
type ForwardRequest struct {
	Message    string    `json:"message"`
	Name       string    `json:"name"`
	SessionKey string    `json:"sessionKey,omitempty"`
	RequestID  string    `json:"-"`
	ReceivedAt time.Time `json:"-"`
}

// RespondRequest is the input for the /v1/responses endpoint.
type RespondRequest struct {
	Input          string
	PrevResponseID string // empty on first message in a channel
	RequestID      string
	ReceivedAt     time.Time
}

// RespondResult carries the response ID (for threading) and the reply text.
type RespondResult struct {
	ResponseID string
	Text       string
}

type Config struct {
	BaseURL        string
	APIKey         string
	Model          string        // model identifier forwarded to /v1/responses
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	HTTPTimeout    time.Duration // for short requests (hooks, file downloads)
	RespondTimeout time.Duration // for /v1/responses calls (agent processing time)
}

func DefaultConfig(baseURL, apiKey string) Config {
	return Config{
		BaseURL:        baseURL,
		APIKey:         apiKey,
		Model:          "anthropic/claude-sonnet-4-6",
		MaxRetries:     4,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		HTTPTimeout:    15 * time.Second,
		RespondTimeout: 120 * time.Second,
	}
}

type Client struct {
	cfg         Config
	http        *http.Client // short timeout — hooks, file downloads
	respondHTTP *http.Client // no client timeout — context deadline governs Respond()
}

func New(cfg Config) *Client {
	if cfg.Model == "" {
		cfg.Model = "anthropic/claude-sonnet-4-6"
	}
	if cfg.RespondTimeout == 0 {
		cfg.RespondTimeout = 120 * time.Second
	}
	return &Client{
		cfg:         cfg,
		http:        &http.Client{Timeout: cfg.HTTPTimeout},
		respondHTTP: &http.Client{},
	}
}

// Respond sends a message to the /v1/responses endpoint and returns the agent
// reply along with the response ID that must be passed as PrevResponseID on the
// next call for the same channel to maintain conversation continuity.
func (c *Client) Respond(ctx context.Context, req RespondRequest) (*RespondResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RespondTimeout)
	defer cancel()

	reqBody := struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		PrevResponseID string `json:"previous_response_id,omitempty"`
	}{
		Model:          c.cfg.Model,
		Input:          req.Input,
		PrevResponseID: req.PrevResponseID,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal respond request: %w", err)
	}

	slog.Info("posting to openclaw responses api",
		"endpoint", c.cfg.BaseURL+"/v1/responses",
		"has_prev_response", req.PrevResponseID != "",
		"request_id", req.RequestID,
		"body", string(body),
	)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build respond request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.respondHTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("respond request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		slog.Error("openclaw respond error",
			"status", resp.StatusCode,
			"body", string(body),
			"request_id", req.RequestID,
		)
		return nil, fmt.Errorf("respond: status %d: %s", resp.StatusCode, string(body))
	}

	var respBody struct {
		ID     string `json:"id"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, fmt.Errorf("decode respond response: %w", err)
	}

	var text string
	for _, item := range respBody.Output {
		if item.Type != "message" {
			continue
		}
		for _, block := range item.Content {
			if block.Type == "output_text" && block.Text != "" {
				text = block.Text
			}
		}
	}

	slog.Debug("openclaw respond result",
		"response_id", respBody.ID,
		"text_len", len(text),
		"request_id", req.RequestID,
	)

	return &RespondResult{ResponseID: respBody.ID, Text: text}, nil
}

// DownloadAndRespond downloads a file attachment from Zoho, saves it to the
// OpenClaw workspace, then forwards a reference message via the responses API.
func (c *Client) DownloadAndRespond(
	ctx context.Context,
	srcURL, filename, mimeType, zohoToken, comment, prevResponseID, workspaceDir string,
) (*RespondResult, error) {
	// --- Download from Zoho ---
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	if zohoToken != "" {
		dlReq.Header.Set("Authorization", "Zoho-oauthtoken "+zohoToken)
	}

	resp, err := c.http.Do(dlReq)
	if err != nil {
		return nil, fmt.Errorf("download file from zoho: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zoho download status: %s", resp.Status)
	}

	// --- Save to workspace ---
	uploadsDir := filepath.Join(workspaceDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		return nil, fmt.Errorf("create uploads dir: %w", err)
	}

	safeFilename := filepath.Base(filename)
	destPath := filepath.Join(uploadsDir, safeFilename)

	f, err := os.Create(destPath)
	if err != nil {
		return nil, fmt.Errorf("create workspace file: %w", err)
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(destPath)
		return nil, fmt.Errorf("write workspace file: %w", err)
	}

	slog.Info("file saved to workspace",
		"filename", safeFilename,
		"bytes", written,
		"dest", destPath,
	)

	// --- Tell the agent to process it ---
	agentPath := "~/workspace/uploads/" + safeFilename
	var message string
	if comment != "" {
		message = fmt.Sprintf("%s\n\n[File saved to workspace: %s | Type: %s]", comment, agentPath, mimeType)
	} else {
		message = fmt.Sprintf("A file has been shared. Please process it: %s (type: %s)", agentPath, mimeType)
	}

	return c.Respond(ctx, RespondRequest{
		Input:          message,
		PrevResponseID: prevResponseID,
	})
}

// Forward sends to the legacy /hooks/agent endpoint. Used only for forwardRaw
// (unparseable payloads where channel is unknown).
func (c *Client) Forward(ctx context.Context, req ForwardRequest) error {
	body, err := json.Marshal(struct {
		Message    string `json:"message"`
		Name       string `json:"name"`
		SessionKey string `json:"sessionKey,omitempty"`
		Deliver    bool   `json:"deliver"`
	}{
		Message:    req.Message,
		Name:       req.Name,
		SessionKey: req.SessionKey,
		Deliver:    true,
	})
	if err != nil {
		return fmt.Errorf("marshal forward request: %w", err)
	}

	slog.Debug("forwarding to openclaw hooks",
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

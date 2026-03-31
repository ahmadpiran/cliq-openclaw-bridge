package zoho

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const senderTimeout = 10 * time.Second

// Sender posts messages back to Zoho Cliq via an incoming webhook URL.
// No OAuth token needed — the webhook URL is self-authenticating.
type Sender struct {
	webhookURL string
	http       *http.Client
}

// NewSender constructs a Sender.
// webhookURL is the Zoho Cliq incoming webhook URL configured for the bot.
func NewSender(webhookURL string) *Sender {
	return &Sender{
		webhookURL: webhookURL,
		http:       &http.Client{Timeout: senderTimeout},
	}
}

type cliqMessagePayload struct {
	Text string `json:"text"`
}

// PostToChannel posts a reply to Zoho Cliq via the incoming webhook.
// The chatID parameter is accepted for interface compatibility but not used —
// the webhook URL already encodes the destination.
func (s *Sender) PostToChannel(ctx context.Context, _ string, text string) error {
	body, err := json.Marshal(cliqMessagePayload{Text: text})
	if err != nil {
		return fmt.Errorf("marshal cliq message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build cliq webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("post cliq webhook: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cliq webhook status %d: %s", resp.StatusCode, string(raw))
	}

	slog.Info("zoho cliq reply sent via webhook", "status", resp.StatusCode)
	return nil
}

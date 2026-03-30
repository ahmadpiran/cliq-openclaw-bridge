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

// Sender posts messages to Zoho Cliq channels using the REST API.
type Sender struct {
	refresher *Refresher
	http      *http.Client
	baseURL   string
}

// NewSender constructs a Sender.
// baseURL is the Zoho Cliq API root, e.g. "https://cliq.zoho.com".
func NewSender(refresher *Refresher, baseURL string) *Sender {
	return &Sender{
		refresher: refresher,
		http:      &http.Client{Timeout: senderTimeout},
		baseURL:   baseURL,
	}
}

type cliqMessagePayload struct {
	Text string `json:"text"`
}

// PostToChannel posts a text message to a Zoho Cliq channel by its unique name.
func (s *Sender) PostToChannel(ctx context.Context, channelName, text string) error {
	token, err := s.refresher.ValidToken(ctx)
	if err != nil {
		return fmt.Errorf("get zoho token for reply: %w", err)
	}

	body, err := json.Marshal(cliqMessagePayload{Text: text})
	if err != nil {
		return fmt.Errorf("marshal cliq message: %w", err)
	}

	url := fmt.Sprintf("%s/api/v2/channelsbyname/%s/message", s.baseURL, channelName)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build cliq message request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Zoho-oauthtoken "+token)

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("post cliq message: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cliq message status %d: %s", resp.StatusCode, string(raw))
	}

	slog.Info("zoho cliq reply sent",
		"channel", channelName,
		"status", resp.StatusCode,
	)
	return nil
}

package zoho

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const senderTimeout = 10 * time.Second
const fileUploadTimeout = 60 * time.Second

// Sender posts messages and files back to Zoho Cliq.
// Both text replies and file uploads use the bot webhook zapikey — no OAuth needed.
//
// Note: Zoho Cliq's external bot API does not expose message IDs or support
// message editing via webhooks, so update-in-place is not possible.
type Sender struct {
	webhookURL string // e.g. https://cliq.zoho.com/api/v2/bots/mybotname/message?zapikey=xxx
	cliqAPIURL string // e.g. https://cliq.zoho.com
	botName    string // extracted from webhookURL
	zapiKey    string // extracted from webhookURL
	http       *http.Client
}

// NewSender constructs a Sender.
// webhookURL must be the full Zoho bot message webhook URL including zapikey.
// cliqAPIURL is the Zoho Cliq API base — leave empty to use https://cliq.zoho.com.
func NewSender(webhookURL, cliqAPIURL string) *Sender {
	if cliqAPIURL == "" {
		cliqAPIURL = "https://cliq.zoho.com"
	}

	botName, zapiKey := parseBotWebhookURL(webhookURL)

	return &Sender{
		webhookURL: webhookURL,
		cliqAPIURL: cliqAPIURL,
		botName:    botName,
		zapiKey:    zapiKey,
		http:       &http.Client{Timeout: senderTimeout},
	}
}

// parseBotWebhookURL extracts the bot name and zapikey from a Zoho bot webhook URL.
// Expected format: https://cliq.zoho.com/api/v2/bots/{botname}/message?zapikey={key}
func parseBotWebhookURL(rawURL string) (botName, zapiKey string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}

	zapiKey = u.Query().Get("zapikey")

	// Extract bot name from path: /api/v2/bots/{botname}/message
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "bots" && i+1 < len(parts) {
			botName = parts[i+1]
			break
		}
	}

	return botName, zapiKey
}

// cliqMessagePayload is the JSON body sent to the Zoho bot message endpoint.
// chat_id targets the reply to a specific user's conversation — without it
// Zoho defaults to the bot owner's chat, causing all replies to land there
// regardless of who sent the original message.
type cliqMessagePayload struct {
	Text   string `json:"text"`
	ChatID string `json:"chat_id,omitempty"`
}

// PostToChannel posts a text reply to the given chat via the bot webhook URL.
// chatID must be the Zoho chat ID (e.g. "CT_...") from the inbound webhook
// payload — this ensures the reply reaches the correct user, not just the
// bot owner.
func (s *Sender) PostToChannel(ctx context.Context, chatID string, text string) error {
	body, err := json.Marshal(cliqMessagePayload{Text: text, ChatID: chatID})
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

	slog.Info("zoho cliq reply sent via webhook",
		"chat_id", chatID,
		"status", resp.StatusCode,
	)
	return nil
}

// SendFile uploads a file to the Zoho Cliq bot using the zapikey from the webhook URL.
// No OAuth token required — the zapikey covers both messages and file uploads.
func (s *Sender) SendFile(ctx context.Context, _ string, filePath string) error {
	if s.zapiKey == "" || s.botName == "" {
		return fmt.Errorf("SendFile: could not extract bot name or zapikey from webhook URL")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file for upload: %w", err)
	}
	defer f.Close()

	filename := filepath.Base(filePath)
	mimeType := mime.TypeByExtension(filepath.Ext(filename))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	h.Set("Content-Type", mimeType)

	part, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create multipart field: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("write file to multipart: %w", err)
	}
	mw.Close()

	uploadURL := fmt.Sprintf("%s/api/v2/bots/%s/files?zapikey=%s",
		s.cliqAPIURL, s.botName, s.zapiKey)

	uploadCtx, cancel := context.WithTimeout(ctx, fileUploadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return fmt.Errorf("build file upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	uploadClient := &http.Client{Timeout: fileUploadTimeout}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return fmt.Errorf("file upload request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("file upload status %d: %s", resp.StatusCode, string(raw))
	}

	slog.Info("zoho cliq file sent",
		"filename", filename,
		"status", resp.StatusCode,
	)
	return nil
}

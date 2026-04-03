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
	"os"
	"path/filepath"
	"time"
)

const senderTimeout = 10 * time.Second
const fileUploadTimeout = 60 * time.Second

// TokenProvider is satisfied by *Refresher.
type SenderTokenProvider interface {
	ValidToken(ctx context.Context) (string, error)
}

// Sender posts messages and files back to Zoho Cliq.
// Text replies use the bot webhook token (no OAuth).
// File uploads use OAuth via the Files REST API.
type Sender struct {
	webhookURL string
	cliqAPIURL string
	refresher  SenderTokenProvider
	http       *http.Client
}

// NewSender constructs a Sender.
// webhookURL is the Zoho Cliq incoming webhook URL for text replies.
// cliqAPIURL is the Zoho Cliq API base (default: https://cliq.zoho.com).
// refresher is used for file upload OAuth — may be nil if file sending is not needed.
func NewSender(webhookURL, cliqAPIURL string, refresher SenderTokenProvider) *Sender {
	if cliqAPIURL == "" {
		cliqAPIURL = "https://cliq.zoho.com"
	}
	return &Sender{
		webhookURL: webhookURL,
		cliqAPIURL: cliqAPIURL,
		refresher:  refresher,
		http:       &http.Client{Timeout: senderTimeout},
	}
}

type cliqMessagePayload struct {
	Text string `json:"text"`
}

// PostToChannel posts a text reply to Zoho Cliq via the incoming webhook.
// chatID is accepted for interface compatibility but not used for text —
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

// SendFile uploads a file to a Zoho Cliq chat via the REST API.
// Requires OAuth — refresher must be non-nil.
// chatID is the Cliq chat ID, e.g. CT_1424569719276050237_918941379-B2.
// filePath is the absolute path to the file on disk.
func (s *Sender) SendFile(ctx context.Context, chatID, filePath string) error {
	if s.refresher == nil {
		return fmt.Errorf("SendFile: no token refresher configured")
	}

	token, err := s.refresher.ValidToken(ctx)
	if err != nil {
		return fmt.Errorf("get oauth token for file upload: %w", err)
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

	// Build multipart body.
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

	uploadURL := fmt.Sprintf("%s/api/v2/chats/%s/files", s.cliqAPIURL, chatID)

	uploadCtx, cancel := context.WithTimeout(ctx, fileUploadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return fmt.Errorf("build file upload request: %w", err)
	}
	req.Header.Set("Authorization", "Zoho-oauthtoken "+token)
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
		"chat_id", chatID,
		"filename", filename,
		"status", resp.StatusCode,
	)
	return nil
}

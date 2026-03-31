package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

// TokenProvider is satisfied by *zoho.Refresher.
type TokenProvider interface {
	ValidToken(ctx context.Context) (string, error)
}

// Forwarder is satisfied by *gateway.Client.
type Forwarder interface {
	Forward(ctx context.Context, req gateway.ForwardRequest) error
	DownloadAndForward(ctx context.Context, srcURL, filename, mimeType, zohoToken, comment, sessionKey, workspaceDir string) error
}

// ZohoSender is satisfied by *zoho.Sender.
type ZohoSender interface {
	PostToChannel(ctx context.Context, chatID, text string) error
}

// zohoMessagePayload is the structure the Deluge script sends to the bridge.
type zohoMessagePayload struct {
	Type    string `json:"type"`
	Message struct {
		Text           string `json:"text"`
		Sender         string `json:"sender"`
		Channel        string `json:"channel"`
		ChannelTitle   string `json:"channel_title"`
		MessageType    string `json:"message_type"`
		AttachmentURL  string `json:"attachment_url"`
		AttachmentName string `json:"attachment_name"`
		AttachmentMime string `json:"attachment_mime"`
	} `json:"message"`
}

// Dispatcher routes jobs from the worker pool to the correct processing path.
// Replies are no longer polled here — they are pushed by OpenClaw's
// message:sent hook to POST /notify, which forwards them to Zoho Cliq directly.
type Dispatcher struct {
	gw           Forwarder
	refresher    TokenProvider
	workspaceDir string
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(
	gw Forwarder,
	refresher TokenProvider,
	workspaceDir string,
) *Dispatcher {
	return &Dispatcher{
		gw:           gw,
		refresher:    refresher,
		workspaceDir: workspaceDir,
	}
}

// Dispatch is the worker.HandlerFunc passed to the pool.
// It forwards the message to OpenClaw and returns immediately.
// Replies come back asynchronously via POST /notify from OpenClaw's hook.
func (d *Dispatcher) Dispatch(ctx context.Context, job worker.Job) error {
	var p zohoMessagePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		slog.Warn("dispatcher: unrecognised payload, forwarding raw",
			"request_id", job.RequestID, "error", err)
		return d.forwardRaw(ctx, job)
	}

	if p.Message.MessageType == "file" && p.Message.AttachmentURL != "" {
		return d.handleFile(ctx, job, p.Message)
	}

	return d.forward(ctx, job, p)
}

func (d *Dispatcher) forward(ctx context.Context, job worker.Job, p zohoMessagePayload) error {
	message := string(job.Payload)
	if p.Message.Text != "" {
		message = fmt.Sprintf("[Zoho Cliq] %s wrote in #%s: %s",
			p.Message.Sender, p.Message.ChannelTitle, p.Message.Text)
	}

	if err := d.gw.Forward(ctx, gateway.ForwardRequest{
		Message:    message,
		Name:       "Zoho Cliq",
		SessionKey: "hook:zoho-cliq",
		RequestID:  job.RequestID,
		ReceivedAt: job.ReceivedAt,
	}); err != nil {
		return fmt.Errorf("forward to openclaw: %w", err)
	}

	slog.Info("dispatcher: job forwarded to openclaw",
		"request_id", job.RequestID)

	return nil
}

func (d *Dispatcher) handleFile(ctx context.Context, job worker.Job, msg struct {
	Text           string `json:"text"`
	Sender         string `json:"sender"`
	Channel        string `json:"channel"`
	ChannelTitle   string `json:"channel_title"`
	MessageType    string `json:"message_type"`
	AttachmentURL  string `json:"attachment_url"`
	AttachmentName string `json:"attachment_name"`
	AttachmentMime string `json:"attachment_mime"`
}) error {
	if d.workspaceDir == "" {
		slog.Warn("dispatcher: OPENCLAW_WORKSPACE_DIR not set, cannot save file",
			"request_id", job.RequestID)
		return nil
	}

	token, err := d.refresher.ValidToken(ctx)
	if err != nil {
		return fmt.Errorf("get zoho token for file download: %w", err)
	}

	slog.Info("dispatcher: downloading and forwarding file",
		"request_id", job.RequestID,
		"filename", msg.AttachmentName,
		"mime_type", msg.AttachmentMime,
	)

	if err := d.gw.DownloadAndForward(
		ctx,
		msg.AttachmentURL,
		msg.AttachmentName,
		msg.AttachmentMime,
		token,
		msg.Text,
		"hook:zoho-cliq",
		d.workspaceDir,
	); err != nil {
		return fmt.Errorf("download and forward: %w", err)
	}

	slog.Info("dispatcher: file forwarded to openclaw",
		"request_id", job.RequestID)

	return nil
}

func (d *Dispatcher) forwardRaw(ctx context.Context, job worker.Job) error {
	return d.gw.Forward(ctx, gateway.ForwardRequest{
		Message:    string(job.Payload),
		Name:       "Zoho Cliq",
		SessionKey: "hook:zoho-cliq",
		RequestID:  job.RequestID,
		ReceivedAt: job.ReceivedAt,
	})
}

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
	StreamFile(ctx context.Context, srcURL, filename, mimeType, zohoToken string) error
}

// zohoAttachmentPayload is used to detect file attachments in the webhook body.
type zohoAttachmentPayload struct {
	Type       string `json:"type"`
	Attachment *struct {
		DownloadURL string `json:"download_url"`
		Name        string `json:"name"`
		MimeType    string `json:"mime_type"`
	} `json:"attachment,omitempty"`
}

// Dispatcher routes jobs from the worker pool to the appropriate gateway path.
// File attachments are streamed via io.Pipe; all other payloads are forwarded as JSON.
type Dispatcher struct {
	gw        Forwarder
	refresher TokenProvider
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(gw Forwarder, refresher TokenProvider) *Dispatcher {
	return &Dispatcher{gw: gw, refresher: refresher}
}

// Dispatch is the worker.HandlerFunc passed to the pool.
// It is the single entry point for all async job processing.
func (d *Dispatcher) Dispatch(ctx context.Context, job worker.Job) error {
	var p zohoAttachmentPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		// Unknown shape — forward raw and let OpenClaw decide.
		slog.Warn("dispatcher: unrecognised payload shape, forwarding raw",
			"request_id", job.RequestID,
			"error", err,
		)
		return d.forward(ctx, job)
	}

	if p.Attachment != nil && p.Attachment.DownloadURL != "" {
		return d.streamAttachment(ctx, job, p.Attachment)
	}

	return d.forward(ctx, job)
}

func (d *Dispatcher) forward(ctx context.Context, job worker.Job) error {
	var p struct {
		Type    string `json:"type"`
		Message struct {
			Text    string `json:"text"`
			Sender  string `json:"sender"`
			Channel string `json:"channel"`
		} `json:"message"`
	}
	_ = json.Unmarshal(job.Payload, &p)

	message := string(job.Payload)
	if p.Message.Text != "" {
		message = fmt.Sprintf(
			"[Zoho Cliq] %s wrote in #%s: %s",
			p.Message.Sender,
			p.Message.Channel,
			p.Message.Text,
		)
	}

	return d.gw.Forward(ctx, gateway.ForwardRequest{
		Message:    message,
		Name:       "Zoho Cliq",
		SessionKey: "hook:zoho-cliq",
		Deliver:    false,
		RequestID:  job.RequestID,
		ReceivedAt: job.ReceivedAt,
	})
}

func (d *Dispatcher) streamAttachment(
	ctx context.Context,
	job worker.Job,
	att *struct {
		DownloadURL string `json:"download_url"`
		Name        string `json:"name"`
		MimeType    string `json:"mime_type"`
	},
) error {
	token, err := d.refresher.ValidToken(ctx)
	if err != nil {
		return fmt.Errorf("get zoho token for file download: %w", err)
	}

	slog.Info("dispatcher: streaming file attachment",
		"request_id", job.RequestID,
		"filename", att.Name,
		"mime_type", att.MimeType,
	)

	return d.gw.StreamFile(ctx, att.DownloadURL, att.Name, att.MimeType, token)
}

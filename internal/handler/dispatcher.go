package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

// SessionReader is satisfied by *session.Reader.
type SessionReader interface {
	FindLatestSessionFile(afterTime time.Time) (string, error)
	WaitForAssistantReply(ctx context.Context, sessionFile string, afterTime time.Time) (string, error)
}

// TokenProvider is satisfied by *zoho.Refresher.
type TokenProvider interface {
	ValidToken(ctx context.Context) (string, error)
}

// Forwarder is satisfied by *gateway.Client.
type Forwarder interface {
	Forward(ctx context.Context, req gateway.ForwardRequest) error
	StreamFile(ctx context.Context, srcURL, filename, mimeType, zohoToken string) error
}

// ZohoSender is satisfied by *zoho.Sender.
type ZohoSender interface {
	PostToChannel(ctx context.Context, channelName, text string) error
}

// zohoMessagePayload extracts the fields we care about from the Zoho webhook body.
type zohoMessagePayload struct {
	Type    string `json:"type"`
	Message struct {
		Text           string `json:"text"`
		Sender         string `json:"sender"`
		Channel        string `json:"channel"`
		ChannelTitle   string `json:"channel_title"`
		MessageType    string `json:"message_type"`   // "text" or "file"
		AttachmentURL  string `json:"attachment_url"` // short-lived Zoho download URL
		AttachmentName string `json:"attachment_name"`
		AttachmentMime string `json:"attachment_mime"`
	} `json:"message"`
}

// Dispatcher routes jobs from the worker pool.
type Dispatcher struct {
	gw            Forwarder
	refresher     TokenProvider
	sender        ZohoSender    // nil = reply-back disabled
	sessionReader SessionReader // nil = reply-back disabled
	replyTimeout  time.Duration
}

// NewDispatcher constructs a Dispatcher.
// sender and sessionReader may be nil to disable reply-back.
func NewDispatcher(
	gw Forwarder,
	refresher TokenProvider,
	sender ZohoSender,
	sessionReader SessionReader,
	replyTimeout time.Duration,
) *Dispatcher {
	return &Dispatcher{
		gw:            gw,
		refresher:     refresher,
		sender:        sender,
		sessionReader: sessionReader,
		replyTimeout:  replyTimeout,
	}
}

// Dispatch is the worker.HandlerFunc passed to the pool.
func (d *Dispatcher) Dispatch(ctx context.Context, job worker.Job) error {
	var p zohoMessagePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		slog.Warn("dispatcher: unrecognised payload, forwarding raw",
			"request_id", job.RequestID, "error", err)
		return d.forwardRaw(ctx, job)
	}

	// File attachment — stream directly to OpenClaw workspace
	if p.Message.MessageType == "file" && p.Message.AttachmentURL != "" {
		return d.streamAttachment(ctx, job, p.Message)
	}

	return d.forward(ctx, job, p)
}

func (d *Dispatcher) forward(ctx context.Context, job worker.Job, p zohoMessagePayload) error {
	message := string(job.Payload)
	if p.Message.Text != "" {
		message = fmt.Sprintf("[Zoho Cliq] %s wrote in #%s: %s",
			p.Message.Sender, p.Message.ChannelTitle, p.Message.Text)
	}

	dispatchTime := time.Now()

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

	if d.sender == nil || d.sessionReader == nil || p.Message.Channel == "" {
		slog.Info("dispatcher: reply-back skipped",
			"has_sender", d.sender != nil,
			"has_reader", d.sessionReader != nil,
			"channel", p.Message.Channel,
		)
		return nil
	}

	replyCtx, cancel := context.WithTimeout(ctx, d.replyTimeout)
	defer cancel()

	// Poll for the session file — OpenClaw may take several seconds to create it.
	sessionFile, err := d.waitForSessionFile(replyCtx, dispatchTime)
	if err != nil {
		slog.Error("dispatcher: session file never appeared",
			"request_id", job.RequestID, "error", err)
		return nil
	}

	slog.Info("dispatcher: found session file",
		"request_id", job.RequestID, "file", sessionFile)

	reply, err := d.sessionReader.WaitForAssistantReply(replyCtx, sessionFile, dispatchTime)
	if err != nil {
		slog.Error("dispatcher: timeout waiting for agent reply",
			"request_id", job.RequestID, "error", err)
		return nil
	}

	slog.Info("dispatcher: agent replied, posting to zoho cliq",
		"request_id", job.RequestID,
		"channel", p.Message.Channel,
		"reply_len", len(reply),
	)

	// OpenClaw returns "NO_REPLY" when it has nothing to say (e.g. file-only message).
	// Do not post this literal string back to the user.
	if reply == "NO_REPLY" || reply == "" {
		slog.Info("dispatcher: agent returned no reply, skipping post",
			"request_id", job.RequestID)
		return nil
	}

	if err := d.sender.PostToChannel(ctx, p.Message.Channel, reply); err != nil {
		slog.Error("dispatcher: failed to post reply",
			"request_id", job.RequestID, "error", err)
	}

	return nil
}

// waitForSessionFile polls FindLatestSessionFile every second until a session
// file modified after afterTime appears or the context expires.
func (d *Dispatcher) waitForSessionFile(ctx context.Context, afterTime time.Time) (string, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for session file: %w", ctx.Err())
		case <-ticker.C:
			f, err := d.sessionReader.FindLatestSessionFile(afterTime)
			if err == nil {
				return f, nil
			}
		}
	}
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

func (d *Dispatcher) streamAttachment(ctx context.Context, job worker.Job, msg struct {
	Text           string `json:"text"`
	Sender         string `json:"sender"`
	Channel        string `json:"channel"`
	ChannelTitle   string `json:"channel_title"`
	MessageType    string `json:"message_type"`
	AttachmentURL  string `json:"attachment_url"`
	AttachmentName string `json:"attachment_name"`
	AttachmentMime string `json:"attachment_mime"`
}) error {
	token, err := d.refresher.ValidToken(ctx)
	if err != nil {
		return fmt.Errorf("get zoho token for file download: %w", err)
	}

	slog.Info("dispatcher: streaming file attachment",
		"request_id", job.RequestID,
		"filename", msg.AttachmentName,
		"mime_type", msg.AttachmentMime,
	)

	return d.gw.StreamFile(ctx, msg.AttachmentURL, msg.AttachmentName, msg.AttachmentMime, token)
}

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

// SessionReader is satisfied by *session.Reader.
type SessionReader interface {
	FindLatestSessionFile(afterTime time.Time) (string, error)
	WaitForAssistantReply(ctx context.Context, sessionFile string, afterTime time.Time) (string, error)
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
type Dispatcher struct {
	gw            Forwarder
	refresher     TokenProvider
	sender        ZohoSender
	sessionReader SessionReader
	replyTimeout  time.Duration
	workspaceDir  string
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(
	gw Forwarder,
	refresher TokenProvider,
	sender ZohoSender,
	sessionReader SessionReader,
	replyTimeout time.Duration,
	workspaceDir string,
) *Dispatcher {
	return &Dispatcher{
		gw:            gw,
		refresher:     refresher,
		sender:        sender,
		sessionReader: sessionReader,
		replyTimeout:  replyTimeout,
		workspaceDir:  workspaceDir,
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

	if p.Message.MessageType == "file" && p.Message.AttachmentURL != "" {
		return d.handleFile(ctx, job, p.Message)
	}

	return d.forward(ctx, job, p)
}

// forward sends a text message to OpenClaw and posts the reply back to Zoho.
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

	return d.postReply(ctx, job, p.Message.Channel, dispatchTime)
}

// handleFile downloads the file to the OpenClaw workspace and asks the agent to process it.
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

	dispatchTime := time.Now()

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

	return d.postReply(ctx, job, msg.Channel, dispatchTime)
}

// postReply waits for the agent reply and posts it back to Zoho Cliq.
// Errors here are soft — the job itself is considered successful.
func (d *Dispatcher) postReply(ctx context.Context, job worker.Job, channel string, afterTime time.Time) error {
	if d.sender == nil || d.sessionReader == nil || channel == "" {
		slog.Info("dispatcher: reply-back skipped",
			"has_sender", d.sender != nil,
			"has_reader", d.sessionReader != nil,
			"channel", channel,
		)
		return nil
	}

	sessionFile, err := d.waitForSessionFile(ctx, afterTime)
	if err != nil {
		slog.Error("dispatcher: session file never appeared",
			"request_id", job.RequestID, "error", err)
		return nil
	}

	slog.Info("dispatcher: found session file",
		"request_id", job.RequestID, "file", sessionFile)

	replyCtx, cancel := context.WithTimeout(ctx, d.replyTimeout)
	defer cancel()

	reply, err := d.sessionReader.WaitForAssistantReply(replyCtx, sessionFile, afterTime)
	if err != nil {
		slog.Error("dispatcher: timeout waiting for agent reply",
			"request_id", job.RequestID, "error", err)
		return nil
	}

	if reply == "" || reply == "NO_REPLY" {
		slog.Info("dispatcher: agent returned no reply, skipping post",
			"request_id", job.RequestID)
		return nil
	}

	slog.Info("dispatcher: agent replied, posting to zoho cliq",
		"request_id", job.RequestID,
		"channel", channel,
		"reply_len", len(reply),
	)

	if err := d.sender.PostToChannel(ctx, channel, reply); err != nil {
		slog.Error("dispatcher: failed to post reply",
			"request_id", job.RequestID, "error", err)
	}

	return nil
}

// forwardRaw forwards an unrecognised payload without reply-back.
func (d *Dispatcher) forwardRaw(ctx context.Context, job worker.Job) error {
	return d.gw.Forward(ctx, gateway.ForwardRequest{
		Message:    string(job.Payload),
		Name:       "Zoho Cliq",
		SessionKey: "hook:zoho-cliq",
		RequestID:  job.RequestID,
		ReceivedAt: job.ReceivedAt,
	})
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

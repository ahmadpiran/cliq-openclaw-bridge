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
	TailAssistantMessages(ctx context.Context, sessionFile string, afterTime time.Time, out chan<- string)
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
// Reply-back runs in a background goroutine so the worker is freed immediately
// after forwarding — long-running tool chains and approvals are handled without
// blocking the pool.
type Dispatcher struct {
	gw            Forwarder
	refresher     TokenProvider
	sender        ZohoSender
	sessionReader SessionReader
	workspaceDir  string
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(
	gw Forwarder,
	refresher TokenProvider,
	sender ZohoSender,
	sessionReader SessionReader,
	workspaceDir string,
) *Dispatcher {
	return &Dispatcher{
		gw:            gw,
		refresher:     refresher,
		sender:        sender,
		sessionReader: sessionReader,
		workspaceDir:  workspaceDir,
	}
}

// Dispatch is the worker.HandlerFunc passed to the pool.
// It forwards the message and returns immediately. Reply-back runs
// in a goroutine that tails the session file until the context expires.
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

// forward sends a text message to OpenClaw and launches reply-back in background.
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

	// Launch reply-back in a goroutine — job returns immediately to the pool.
	// The goroutine tails the session file until ctx expires (WORKER_JOB_TIMEOUT).
	// No idle timeout: tool approvals and long-running tasks all come through.
	go d.postReply(ctx, job.RequestID, p.Message.Channel, dispatchTime)

	return nil
}

// handleFile downloads the file to workspace and asks the agent to process it.
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

	slog.Info("dispatcher: file forwarded to openclaw",
		"request_id", job.RequestID)

	go d.postReply(ctx, job.RequestID, msg.Channel, dispatchTime)

	return nil
}

// postReply runs in a goroutine. It waits for the session file to appear,
// then tails it — posting every new assistant message to Zoho Cliq until
// ctx is cancelled. ctx comes from the worker pool and expires at WORKER_JOB_TIMEOUT,
// which is the only deadline. There is no idle timeout.
func (d *Dispatcher) postReply(ctx context.Context, requestID, channel string, afterTime time.Time) {
	if d.sender == nil || d.sessionReader == nil || channel == "" {
		slog.Info("dispatcher: reply-back skipped",
			"has_sender", d.sender != nil,
			"has_reader", d.sessionReader != nil,
			"channel", channel,
		)
		return
	}

	// Poll until session file appears.
	fileTicker := time.NewTicker(1 * time.Second)
	defer fileTicker.Stop()

	var sessionFile string
	for {
		select {
		case <-ctx.Done():
			return
		case <-fileTicker.C:
			f, err := d.sessionReader.FindLatestSessionFile(afterTime)
			if err == nil {
				sessionFile = f
				goto found
			}
		}
	}

found:
	slog.Info("dispatcher: found session file, tailing for replies",
		"request_id", requestID, "file", sessionFile)

	out := make(chan string, 8)
	go d.sessionReader.TailAssistantMessages(ctx, sessionFile, afterTime, out)

	for {
		select {
		case reply, ok := <-out:
			if !ok {
				return
			}
			if reply == "" || reply == "NO_REPLY" {
				continue
			}
			slog.Info("dispatcher: agent replied, posting to zoho cliq",
				"request_id", requestID,
				"channel", channel,
				"reply_len", len(reply),
			)
			if err := d.sender.PostToChannel(ctx, channel, reply); err != nil {
				slog.Error("dispatcher: failed to post reply",
					"request_id", requestID, "error", err)
			}
		case <-ctx.Done():
			slog.Info("dispatcher: reply goroutine context expired",
				"request_id", requestID)
			return
		}
	}
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

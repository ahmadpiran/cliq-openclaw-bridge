package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

type TokenProvider interface {
	ValidToken(ctx context.Context) (string, error)
}

type Forwarder interface {
	Forward(ctx context.Context, req gateway.ForwardRequest) error
	DownloadAndForward(ctx context.Context, srcURL, filename, mimeType, zohoToken, comment, sessionKey, workspaceDir string) error
}

type ZohoSender interface {
	PostToChannel(ctx context.Context, chatID, text string) error
}

type SessionReader interface {
	FindLatestSessionFile(afterTime time.Time) (string, error)
	TailAssistantMessages(ctx context.Context, sessionFile string, afterTime time.Time, out chan<- string)
}

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
// claimedSessions prevents two reply goroutines from tailing the same session
// file simultaneously, which would cause duplicate messages in Zoho Cliq.
type Dispatcher struct {
	gw              Forwarder
	refresher       TokenProvider
	sender          ZohoSender
	sessionReader   SessionReader
	workspaceDir    string
	replyTimeout    time.Duration
	claimedSessions sync.Map // map[string]struct{} — session file path → claimed
}

func NewDispatcher(
	gw Forwarder,
	refresher TokenProvider,
	sender ZohoSender,
	sessionReader SessionReader,
	workspaceDir string,
	replyTimeout time.Duration,
) *Dispatcher {
	return &Dispatcher{
		gw:            gw,
		refresher:     refresher,
		sender:        sender,
		sessionReader: sessionReader,
		workspaceDir:  workspaceDir,
		replyTimeout:  replyTimeout,
	}
}

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

	go d.postReply(job.RequestID, p.Message.Channel, dispatchTime)

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

	go d.postReply(job.RequestID, msg.Channel, dispatchTime)

	return nil
}

// postReply runs in a goroutine with its own context derived from Background.
// It polls for an unclaimed session file, claims it exclusively, tails it for
// replies, then releases the claim when done.
func (d *Dispatcher) postReply(requestID, channel string, afterTime time.Time) {
	if d.sender == nil || d.sessionReader == nil || channel == "" {
		slog.Info("dispatcher: reply-back skipped",
			"has_sender", d.sender != nil,
			"has_reader", d.sessionReader != nil,
			"channel", channel,
		)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), d.replyTimeout)
	defer cancel()

	fileTicker := time.NewTicker(1 * time.Second)
	defer fileTicker.Stop()

	// tryClaimFile attempts to find a session file created after afterTime and
	// claim it exclusively. Returns the path on success, empty string otherwise.
	tryClaimFile := func() string {
		f, err := d.sessionReader.FindLatestSessionFile(afterTime)
		if err != nil {
			return ""
		}
		if _, alreadyClaimed := d.claimedSessions.LoadOrStore(f, struct{}{}); alreadyClaimed {
			slog.Debug("dispatcher: session file already claimed, waiting for new one",
				"request_id", requestID, "file", f)
			return ""
		}
		return f
	}

	var sessionFile string

	// Try immediately before waiting for the first tick. This keeps the
	// reply-back goroutine fast in tests and in production when the agent
	// responds quickly (within the first polling window).
	if f := tryClaimFile(); f != "" {
		sessionFile = f
		goto found
	}

	for {
		select {
		case <-ctx.Done():
			slog.Warn("dispatcher: timed out waiting for unclaimed session file",
				"request_id", requestID)
			return
		case <-fileTicker.C:
			if f := tryClaimFile(); f != "" {
				sessionFile = f
				goto found
			}
		}
	}

found:
	defer d.claimedSessions.Delete(sessionFile)

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
			slog.Info("dispatcher: reply timeout reached",
				"request_id", requestID)
			return
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

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

// ZohoSender is satisfied by *zoho.Sender.
type ZohoSender interface {
	PostToChannel(ctx context.Context, chatID, text string) error
	SendFile(ctx context.Context, chatID, filePath string) error
}

type SessionReader interface {
	FindLatestSessionFile(afterTime time.Time, sessionKey string) (string, error)
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

// parsedReply holds the text and optional file path extracted from an agent reply.
type parsedReply struct {
	text     string
	filePath string // absolute path on disk, empty if no file
}

// parseReply extracts [FILE:path] tags from a reply.
// The agent writes ~/workspace/... paths; we resolve them against workspaceDir.
// Text with the tag stripped is returned alongside the resolved file path.
func parseReply(reply, workspaceDir string) parsedReply {
	const prefix = "[FILE:"
	const suffix = "]"

	idx := strings.Index(reply, prefix)
	if idx == -1 {
		return parsedReply{text: strings.TrimSpace(reply)}
	}

	end := strings.Index(reply[idx:], suffix)
	if end == -1 {
		return parsedReply{text: strings.TrimSpace(reply)}
	}

	rawPath := strings.TrimSpace(reply[idx+len(prefix) : idx+end])

	resolved := rawPath
	if strings.HasPrefix(rawPath, "~/workspace/") {
		// Join with forward slash regardless of OS — container paths are Linux.
		resolved = workspaceDir + "/" + strings.TrimPrefix(rawPath, "~/workspace/")
	}

	text := strings.TrimSpace(reply[:idx] + reply[idx+end+len(suffix):])

	return parsedReply{text: text, filePath: resolved}
}

// sessionKeyForChannel derives a stable, per-channel OpenClaw session key.
// Using the Cliq channel ID ensures each DM thread gets its own AI context
// and conversations do not bleed into one another.
// Falls back to the legacy shared key only when channel is unknown (e.g. raw
// payloads that could not be parsed).
func sessionKeyForChannel(channelID string) string {
	if channelID == "" {
		return "hook:zoho-cliq"
	}
	return "hook:zoho-cliq:" + channelID
}

type Dispatcher struct {
	gw              Forwarder
	refresher       TokenProvider
	sender          ZohoSender
	sessionReader   SessionReader
	workspaceDir    string
	replyTimeout    time.Duration
	claimedSessions sync.Map
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
	sessionKey := sessionKeyForChannel(p.Message.Channel)

	message := string(job.Payload)
	if p.Message.Text != "" {
		message = fmt.Sprintf("[Zoho Cliq] %s wrote in #%s: %s",
			p.Message.Sender, p.Message.ChannelTitle, p.Message.Text)
	}

	dispatchTime := time.Now()

	if err := d.gw.Forward(ctx, gateway.ForwardRequest{
		Message:    message,
		Name:       "Zoho Cliq",
		SessionKey: sessionKey,
		RequestID:  job.RequestID,
		ReceivedAt: job.ReceivedAt,
	}); err != nil {
		return fmt.Errorf("forward to openclaw: %w", err)
	}

	slog.Info("dispatcher: job forwarded to openclaw",
		"request_id", job.RequestID,
		"session_key", sessionKey,
	)

	go d.postReply(job.RequestID, p.Message.Channel, sessionKey, dispatchTime)

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

	sessionKey := sessionKeyForChannel(msg.Channel)

	slog.Info("dispatcher: downloading and forwarding file",
		"request_id", job.RequestID,
		"filename", msg.AttachmentName,
		"mime_type", msg.AttachmentMime,
		"session_key", sessionKey,
	)

	dispatchTime := time.Now()

	if err := d.gw.DownloadAndForward(
		ctx,
		msg.AttachmentURL,
		msg.AttachmentName,
		msg.AttachmentMime,
		token,
		msg.Text,
		sessionKey,
		d.workspaceDir,
	); err != nil {
		return fmt.Errorf("download and forward: %w", err)
	}

	slog.Info("dispatcher: file forwarded to openclaw",
		"request_id", job.RequestID,
		"session_key", sessionKey,
	)

	go d.postReply(job.RequestID, msg.Channel, sessionKey, dispatchTime)

	return nil
}

func (d *Dispatcher) postReply(requestID, channel, sessionKey string, afterTime time.Time) {
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

	tryClaimFile := func() string {
		f, err := d.sessionReader.FindLatestSessionFile(afterTime, sessionKey)
		if err != nil {
			return ""
		}
		if _, alreadyClaimed := d.claimedSessions.LoadOrStore(f, struct{}{}); alreadyClaimed {
			return ""
		}
		return f
	}

	var sessionFile string

	if f := tryClaimFile(); f != "" {
		sessionFile = f
		goto found
	}

	for {
		select {
		case <-ctx.Done():
			slog.Warn("dispatcher: timed out waiting for unclaimed session file",
				"request_id", requestID,
				"session_key", sessionKey,
			)
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
		"request_id", requestID,
		"session_key", sessionKey,
		"file", sessionFile,
	)

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
			d.deliverReply(ctx, requestID, channel, reply)
		case <-ctx.Done():
			slog.Info("dispatcher: reply timeout reached",
				"request_id", requestID,
				"session_key", sessionKey,
			)
			return
		}
	}
}

// deliverReply sends text and/or a file from a single agent reply.
func (d *Dispatcher) deliverReply(ctx context.Context, requestID, channel, reply string) {
	parsed := parseReply(reply, d.workspaceDir)

	// Send text part first if present.
	if parsed.text != "" {
		slog.Info("dispatcher: agent replied, posting to zoho cliq",
			"request_id", requestID,
			"channel", channel,
			"reply_len", len(parsed.text),
		)
		if err := d.sender.PostToChannel(ctx, channel, parsed.text); err != nil {
			slog.Error("dispatcher: failed to post text reply",
				"request_id", requestID, "error", err)
		}
	}

	// Send file part if present.
	if parsed.filePath != "" {
		slog.Info("dispatcher: sending file to zoho cliq",
			"request_id", requestID,
			"channel", channel,
			"file", parsed.filePath,
		)
		if err := d.sender.SendFile(ctx, channel, parsed.filePath); err != nil {
			slog.Error("dispatcher: failed to send file",
				"request_id", requestID,
				"file", parsed.filePath,
				"error", err,
			)
		}
	}
}

func (d *Dispatcher) forwardRaw(ctx context.Context, job worker.Job) error {
	// Channel is unknown for unparseable payloads; fall back to the shared key.
	return d.gw.Forward(ctx, gateway.ForwardRequest{
		Message:    string(job.Payload),
		Name:       "Zoho Cliq",
		SessionKey: sessionKeyForChannel(""),
		RequestID:  job.RequestID,
		ReceivedAt: job.ReceivedAt,
	})
}

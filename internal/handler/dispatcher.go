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

// Forwarder is implemented by *gateway.Client.
// Respond sends a message via the /v1/responses API and returns the reply plus
// a response ID that must be threaded into the next call for this channel.
type Forwarder interface {
	Respond(ctx context.Context, req gateway.RespondRequest) (*gateway.RespondResult, error)
	DownloadAndRespond(ctx context.Context, srcURL, filename, mimeType, zohoToken, comment, prevResponseID, workspaceDir string) (*gateway.RespondResult, error)
}

// SessionReader is satisfied by *session.Reader.
// FindLatestSessionFile locates the JSONL file OpenClaw is writing for the
// current request; TailToolCalls streams tool-call names as they appear.
type SessionReader interface {
	FindLatestSessionFile(afterTime time.Time, sessionKey string) (string, error)
	TailToolCalls(ctx context.Context, sessionFile string, afterTime time.Time, out chan<- string)
}

// ZohoSender is satisfied by *zoho.Sender.
type ZohoSender interface {
	PostToChannel(ctx context.Context, userID, text string) error
	SendFile(ctx context.Context, userID, filePath string) error
}

type zohoMessagePayload struct {
	Type    string `json:"type"`
	Message struct {
		Text           string `json:"text"`
		Sender         string `json:"sender"`
		SenderID       string `json:"sender_id"`
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

type Dispatcher struct {
	gw                 Forwarder
	refresher          TokenProvider
	sender             ZohoSender
	workspaceDir       string
	replyTimeout       time.Duration
	channelResponseIDs sync.Map // channelID string → last response ID string
	sessionReader      SessionReader // optional; nil disables real-time tool streaming
}

func NewDispatcher(
	gw Forwarder,
	refresher TokenProvider,
	sender ZohoSender,
	workspaceDir string,
	replyTimeout time.Duration,
) *Dispatcher {
	return &Dispatcher{
		gw:           gw,
		refresher:    refresher,
		sender:       sender,
		workspaceDir: workspaceDir,
		replyTimeout: replyTimeout,
	}
}

// SetSessionReader enables real-time tool call streaming via JSONL tailing.
// Call after NewDispatcher; safe to call at most once before serving traffic.
func (d *Dispatcher) SetSessionReader(sr SessionReader) {
	d.sessionReader = sr
}

// streamToolCalls tails the session JSONL file OpenClaw writes for the current
// request and posts each tool call name to Zoho Cliq as it fires. Exits when
// ctx is cancelled (caller cancels after gw.Respond returns).
func (d *Dispatcher) streamToolCalls(ctx context.Context, requestID, userID string, afterTime time.Time) {
	// Retry finding the session file — it appears shortly after the first tool
	// call, which may be a second or two into processing.
	var sessionFile string
	for attempt := 0; attempt < 10; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		f, err := d.sessionReader.FindLatestSessionFile(afterTime, "")
		if err == nil {
			sessionFile = f
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if sessionFile == "" {
		slog.Debug("streamToolCalls: no session file found",
			"request_id", requestID)
		return
	}

	slog.Debug("streamToolCalls: tailing session file",
		"request_id", requestID,
		"file", sessionFile)

	toolNames := make(chan string, 16)
	go d.sessionReader.TailToolCalls(ctx, sessionFile, afterTime, toolNames)

	for {
		select {
		case <-ctx.Done():
			return
		case name := <-toolNames:
			slog.Info("dispatcher: tool call in progress",
				"request_id", requestID,
				"tool", name)
			if err := d.sender.PostToChannel(ctx, userID, "⚙️ "+name); err != nil {
				slog.Error("dispatcher: failed to post tool call notification",
					"request_id", requestID, "error", err)
			}
		}
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

func (d *Dispatcher) prevResponseID(channelID string) string {
	if v, ok := d.channelResponseIDs.Load(channelID); ok {
		return v.(string)
	}
	return ""
}

func (d *Dispatcher) forward(ctx context.Context, job worker.Job, p zohoMessagePayload) error {
	channelID := p.Message.Channel

	message := string(job.Payload)
	if p.Message.Text != "" {
		message = fmt.Sprintf("[Zoho Cliq] %s wrote in #%s: %s",
			p.Message.Sender, p.Message.ChannelTitle, p.Message.Text)
	}

	prevID := d.prevResponseID(channelID)
	dispatchTime := time.Now()

	// Stream tool calls in real time while the agent is processing.
	var toolCancel context.CancelFunc
	if d.sessionReader != nil && d.sender != nil && p.Message.SenderID != "" {
		var toolCtx context.Context
		toolCtx, toolCancel = context.WithCancel(ctx)
		go d.streamToolCalls(toolCtx, job.RequestID, p.Message.SenderID, dispatchTime)
	}

	result, err := d.gw.Respond(ctx, gateway.RespondRequest{
		Input:          message,
		PrevResponseID: prevID,
		RequestID:      job.RequestID,
		ReceivedAt:     job.ReceivedAt,
	})
	if toolCancel != nil {
		toolCancel()
	}
	if err != nil {
		return fmt.Errorf("openclaw respond: %w", err)
	}

	if result.ResponseID != "" {
		d.channelResponseIDs.Store(channelID, result.ResponseID)
	}

	slog.Info("dispatcher: openclaw responded",
		"request_id", job.RequestID,
		"response_id", result.ResponseID,
		"continued_thread", prevID != "",
	)

	if d.sender != nil && p.Message.SenderID != "" {
		d.deliverResult(ctx, job.RequestID, p.Message.SenderID, result)
	}

	return nil
}

func (d *Dispatcher) handleFile(ctx context.Context, job worker.Job, msg struct {
	Text           string `json:"text"`
	Sender         string `json:"sender"`
	SenderID       string `json:"sender_id"`
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

	prevID := d.prevResponseID(msg.Channel)

	slog.Info("dispatcher: downloading and forwarding file",
		"request_id", job.RequestID,
		"filename", msg.AttachmentName,
		"mime_type", msg.AttachmentMime,
		"continued_thread", prevID != "",
	)

	result, err := d.gw.DownloadAndRespond(
		ctx,
		msg.AttachmentURL,
		msg.AttachmentName,
		msg.AttachmentMime,
		token,
		msg.Text,
		prevID,
		d.workspaceDir,
	)
	if err != nil {
		return fmt.Errorf("download and respond: %w", err)
	}

	if result.ResponseID != "" {
		d.channelResponseIDs.Store(msg.Channel, result.ResponseID)
	}

	slog.Info("dispatcher: openclaw responded to file",
		"request_id", job.RequestID,
		"response_id", result.ResponseID,
	)

	if d.sender != nil && msg.SenderID != "" {
		d.deliverResult(ctx, job.RequestID, msg.SenderID, result)
	}

	return nil
}

// deliverResult sends a tool-call summary (if any) followed by the agent reply.
func (d *Dispatcher) deliverResult(ctx context.Context, requestID, userID string, result *gateway.RespondResult) {
	if len(result.ToolCalls) > 0 {
		summary := "⚙️ " + strings.Join(result.ToolCalls, " · ")
		if err := d.sender.PostToChannel(ctx, userID, summary); err != nil {
			slog.Error("dispatcher: failed to post tool summary",
				"request_id", requestID, "error", err)
		}
	}
	if result.Text != "" {
		d.deliverReply(ctx, requestID, userID, result.Text)
	}
}

// deliverReply sends text and/or a file from a single agent reply.
func (d *Dispatcher) deliverReply(ctx context.Context, requestID, userID, reply string) {
	parsed := parseReply(reply, d.workspaceDir)

	if parsed.text != "" {
		slog.Info("dispatcher: agent replied, posting to zoho cliq",
			"request_id", requestID,
			"user_id", userID,
			"reply_len", len(parsed.text),
		)
		if err := d.sender.PostToChannel(ctx, userID, parsed.text); err != nil {
			slog.Error("dispatcher: failed to post text reply",
				"request_id", requestID, "error", err)
		}
	}

	if parsed.filePath != "" {
		slog.Info("dispatcher: sending file to zoho cliq",
			"request_id", requestID,
			"user_id", userID,
			"file", parsed.filePath,
		)
		if err := d.sender.SendFile(ctx, userID, parsed.filePath); err != nil {
			slog.Error("dispatcher: failed to send file",
				"request_id", requestID,
				"file", parsed.filePath,
				"error", err,
			)
		}
	}
}

func (d *Dispatcher) forwardRaw(ctx context.Context, job worker.Job) error {
	// Channel unknown — start a new thread with no continuity.
	result, err := d.gw.Respond(ctx, gateway.RespondRequest{
		Input:      string(job.Payload),
		RequestID:  job.RequestID,
		ReceivedAt: job.ReceivedAt,
	})
	if err != nil {
		return fmt.Errorf("respond (raw): %w", err)
	}
	if result.Text != "" {
		slog.Info("dispatcher: raw payload got response (no user to deliver to)",
			"request_id", job.RequestID,
			"response_id", result.ResponseID,
		)
	}
	return nil
}

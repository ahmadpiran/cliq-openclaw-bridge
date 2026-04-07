package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/handler"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

// --- fakes ---

type fakeForwarder struct {
	forwardErr     error
	forwardFileErr error
	forwarded      atomic.Int32
	fileForwarded  atomic.Int32
	lastSessionKey string
}

func (f *fakeForwarder) Forward(_ context.Context, req gateway.ForwardRequest) error {
	f.forwarded.Add(1)
	f.lastSessionKey = req.SessionKey
	return f.forwardErr
}

func (f *fakeForwarder) DownloadAndForward(_ context.Context, _, _, _, _, _, sessionKey, _ string) error {
	f.fileForwarded.Add(1)
	f.lastSessionKey = sessionKey
	return f.forwardFileErr
}

type fakeTokenProvider struct {
	token string
	err   error
}

func (f *fakeTokenProvider) ValidToken(_ context.Context) (string, error) {
	return f.token, f.err
}

type fakeZohoSender struct {
	postErr         error
	fileErr         error
	placeholderErr  error
	updateErr       error
	sent            atomic.Int32
	filesSent       atomic.Int32
	placeholders    atomic.Int32
	updates         atomic.Int32
	lastChannel     string
	lastText        string
	lastFilePath    string
	lastUpdatedText string
	placeholderID   string // returned by SendPlaceholder
}

func (f *fakeZohoSender) PostToChannel(_ context.Context, channel, text string) error {
	f.sent.Add(1)
	f.lastChannel = channel
	f.lastText = text
	return f.postErr
}

func (f *fakeZohoSender) SendPlaceholder(_ context.Context, channel, _ string) (string, error) {
	f.placeholders.Add(1)
	f.lastChannel = channel
	return f.placeholderID, f.placeholderErr
}

func (f *fakeZohoSender) UpdateMessage(_ context.Context, channel, _ string, text string) error {
	f.updates.Add(1)
	f.lastChannel = channel
	f.lastUpdatedText = text
	return f.updateErr
}

func (f *fakeZohoSender) SendFile(_ context.Context, channel, filePath string) error {
	f.filesSent.Add(1)
	f.lastChannel = channel
	f.lastFilePath = filePath
	return f.fileErr
}

type fakeSessionReader struct {
	reply          string
	replyErr       error
	fileErr        error
	calls          atomic.Int32
	lastSessionKey string
}

func (f *fakeSessionReader) FindLatestSessionFile(_ time.Time, sessionKey string) (string, error) {
	f.lastSessionKey = sessionKey
	return "fake.jsonl", f.fileErr
}

func (f *fakeSessionReader) TailAssistantMessages(
	ctx context.Context, _ string, _ time.Time, out chan<- string,
) {
	f.calls.Add(1)
	if f.replyErr != nil || f.reply == "" {
		return
	}
	select {
	case out <- f.reply:
	case <-ctx.Done():
	}
}

// blockingSessionReader returns the same file but blocks until released,
// used to hold a claim open while testing the exclusion behaviour.
type blockingSessionReader struct {
	file    string
	fileErr error
	reply   string
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingSessionReader) FindLatestSessionFile(_ time.Time, _ string) (string, error) {
	return b.file, b.fileErr
}

func (b *blockingSessionReader) TailAssistantMessages(
	ctx context.Context, _ string, _ time.Time, out chan<- string,
) {
	b.calls.Add(1)
	select {
	case <-b.release:
		if b.reply != "" {
			select {
			case out <- b.reply:
			case <-ctx.Done():
			}
		}
	case <-ctx.Done():
	}
}

// --- helpers ---

func newDispatcher(
	gw handler.Forwarder,
	tp handler.TokenProvider,
	sender handler.ZohoSender,
	reader handler.SessionReader,
) *handler.Dispatcher {
	return handler.NewDispatcher(gw, tp, sender, reader, "", 5*time.Second)
}

func makeJob(t *testing.T, payload any) worker.Job {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return worker.Job{RequestID: "req-test", Payload: b, ReceivedAt: time.Now()}
}

// --- tests ---

func TestDispatcher_ForwardsPlainPayload(t *testing.T) {
	gw := &fakeForwarder{}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, nil)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hello", "sender": "Ross",
			"message_type": "text",
		},
	})
	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gw.forwarded.Load() != 1 {
		t.Errorf("expected 1 forward call, got %d", gw.forwarded.Load())
	}
	if gw.fileForwarded.Load() != 0 {
		t.Error("DownloadAndForward must not be called for plain text")
	}
}

func TestDispatcher_ForwardsRawWhenPayloadNotJSON(t *testing.T) {
	gw := &fakeForwarder{}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, nil)

	job := worker.Job{RequestID: "req-raw", Payload: []byte(`NOT_JSON`), ReceivedAt: time.Now()}
	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gw.forwarded.Load() != 1 {
		t.Errorf("expected raw payload to be forwarded, got %d", gw.forwarded.Load())
	}
}

func TestDispatcher_DownloadsAndForwardsFileAttachment(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{token: "zoho-tok-123"}
	d := handler.NewDispatcher(gw, tp, nil, nil, "/tmp/workspace", 5*time.Second)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text":            "check this file",
			"sender":          "Ross",
			"channel":         "CT_123",
			"message_type":    "file",
			"attachment_url":  "https://zoho.example.com/file/42",
			"attachment_name": "report.pdf",
			"attachment_mime": "application/pdf",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gw.fileForwarded.Load() != 1 {
		t.Errorf("expected 1 DownloadAndForward call, got %d", gw.fileForwarded.Load())
	}
	if gw.forwarded.Load() != 0 {
		t.Error("Forward must not be called directly for file attachments")
	}
}

func TestDispatcher_ErrorWhenTokenProviderFails(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{err: errors.New("bolt read error")}
	d := handler.NewDispatcher(gw, tp, nil, nil, "/tmp/workspace", 5*time.Second)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"message_type":    "file",
			"attachment_url":  "https://zoho.example.com/file/99",
			"attachment_name": "file.zip",
			"attachment_mime": "application/zip",
		},
	})

	if err := d.Dispatch(context.Background(), job); err == nil {
		t.Fatal("expected error when token provider fails, got nil")
	}
}

func TestDispatcher_SendsPlaceholderAndUpdatesWithReply(t *testing.T) {
	// Happy path: placeholder is sent first, then updated with the real reply.
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{placeholderID: "msg-placeholder-001"}
	reader := &fakeSessionReader{reply: "Hey Ross! 👋"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text":          "hi",
			"sender":        "Ross",
			"channel":       "CT_123",
			"channel_title": "Test Channel",
			"message_type":  "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	if sender.placeholders.Load() != 1 {
		t.Errorf("expected 1 placeholder sent, got %d", sender.placeholders.Load())
	}
	if sender.updates.Load() != 1 {
		t.Errorf("expected 1 UpdateMessage call, got %d", sender.updates.Load())
	}
	if sender.lastUpdatedText != "Hey Ross! 👋" {
		t.Errorf("wrong updated text: %s", sender.lastUpdatedText)
	}
	// PostToChannel must NOT be called — we updated instead.
	if sender.sent.Load() != 0 {
		t.Errorf("PostToChannel must not be called when placeholder update succeeds, got %d", sender.sent.Load())
	}
	if sender.lastChannel != "CT_123" {
		t.Errorf("wrong channel: %s", sender.lastChannel)
	}
}

func TestDispatcher_FallsBackToPostWhenPlaceholderFails(t *testing.T) {
	// If SendPlaceholder fails, postReply should still deliver the reply via PostToChannel.
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{placeholderErr: errors.New("oauth unavailable")}
	reader := &fakeSessionReader{reply: "Hey!"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if sender.placeholders.Load() != 1 {
		t.Errorf("SendPlaceholder should have been attempted, got %d", sender.placeholders.Load())
	}
	if sender.updates.Load() != 0 {
		t.Error("UpdateMessage must not be called when placeholder ID is empty")
	}
	if sender.sent.Load() != 1 {
		t.Errorf("expected fallback PostToChannel, got %d", sender.sent.Load())
	}
	if sender.lastText != "Hey!" {
		t.Errorf("wrong fallback text: %s", sender.lastText)
	}
}

func TestDispatcher_FallsBackToPostWhenUpdateFails(t *testing.T) {
	// If UpdateMessage fails, postReply should fall back to PostToChannel.
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{
		placeholderID: "msg-001",
		updateErr:     errors.New("message not found"),
	}
	reader := &fakeSessionReader{reply: "Hello!"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if sender.updates.Load() != 1 {
		t.Errorf("UpdateMessage should have been attempted once, got %d", sender.updates.Load())
	}
	if sender.sent.Load() != 1 {
		t.Errorf("expected 1 fallback PostToChannel call, got %d", sender.sent.Load())
	}
	if sender.lastText != "Hello!" {
		t.Errorf("wrong fallback text: %s", sender.lastText)
	}
}

func TestDispatcher_SessionKeyIsPerChannel(t *testing.T) {
	gw := &fakeForwarder{}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, nil)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text":         "hello",
			"sender":       "Ross",
			"channel":      "CT_abc999",
			"message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "hook:zoho-cliq:CT_abc999"
	if gw.lastSessionKey != want {
		t.Errorf("expected session key %q, got %q", want, gw.lastSessionKey)
	}
}

func TestDispatcher_SessionKeyFallsBackWhenChannelEmpty(t *testing.T) {
	gw := &fakeForwarder{}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, nil)

	job := worker.Job{RequestID: "req-raw", Payload: []byte(`NOT_JSON`), ReceivedAt: time.Now()}
	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "hook:zoho-cliq"
	if gw.lastSessionKey != want {
		t.Errorf("expected fallback session key %q, got %q", want, gw.lastSessionKey)
	}
}

func TestDispatcher_SessionKeyPassedToSessionReader(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{placeholderID: "ph-1"}
	reader := &fakeSessionReader{reply: "ok"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text":         "hi",
			"sender":       "Ross",
			"channel":      "CT_xyz",
			"message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	want := "hook:zoho-cliq:CT_xyz"
	if reader.lastSessionKey != want {
		t.Errorf("expected session reader to receive key %q, got %q", want, reader.lastSessionKey)
	}
}

func TestDispatcher_NoDuplicatesWhenTwoJobsRaceForSameSessionFile(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{placeholderID: "ph-race"}
	release := make(chan struct{})
	reader := &blockingSessionReader{
		file:    "shared.jsonl",
		reply:   "single reply",
		release: release,
	}

	d := handler.NewDispatcher(gw, &fakeTokenProvider{}, sender, reader, "", 3*time.Second)

	jobA := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "first", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})
	jobB := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "second", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})

	_ = d.Dispatch(context.Background(), jobA)
	time.Sleep(100 * time.Millisecond)
	_ = d.Dispatch(context.Background(), jobB)
	time.Sleep(100 * time.Millisecond)

	close(release)
	time.Sleep(300 * time.Millisecond)

	// One placeholder per job, but only one reply delivered.
	totalDeliveries := sender.updates.Load() + sender.sent.Load()
	if totalDeliveries != 1 {
		t.Errorf("expected exactly 1 reply delivered, got %d (duplicate suppression failed)", totalDeliveries)
	}
}

func TestDispatcher_NoReplyWhenSenderIsNil(t *testing.T) {
	gw := &fakeForwarder{}
	reader := &fakeSessionReader{reply: "some reply"}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, reader)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if reader.calls.Load() != 0 {
		t.Error("session reader must not be called when sender is nil")
	}
}

func TestDispatcher_NoReplyWhenChannelIsEmpty(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{}
	reader := &fakeSessionReader{reply: "some reply"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if sender.placeholders.Load() != 0 {
		t.Error("placeholder must not be sent when channel is empty")
	}
	if sender.updates.Load()+sender.sent.Load() != 0 {
		t.Error("reply must not be sent when channel is empty")
	}
}

func TestDispatcher_ForwardErrorPropagates(t *testing.T) {
	gw := &fakeForwarder{forwardErr: errors.New("openclaw down")}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, nil)

	job := makeJob(t, map[string]any{
		"type":    "message",
		"message": map[string]any{"text": "hi", "message_type": "text"},
	})

	if err := d.Dispatch(context.Background(), job); err == nil {
		t.Fatal("expected error when forward fails, got nil")
	}
}

func TestDispatcher_ReplyBackSilentOnSenderFailure(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{
		placeholderID: "ph-fail",
		updateErr:     errors.New("zoho api down"),
		postErr:       errors.New("zoho api down"),
	}
	reader := &fakeSessionReader{reply: "Hey!"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("sender failure should not fail the job, got: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	// Both update and fallback post were attempted.
	if sender.updates.Load() != 1 {
		t.Errorf("UpdateMessage should have been called once, got %d", sender.updates.Load())
	}
	if sender.sent.Load() != 1 {
		t.Errorf("fallback PostToChannel should have been called once, got %d", sender.sent.Load())
	}
}

func TestDispatcher_SendsFileWhenReplyContainsFileTag(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{placeholderID: "ph-file"}
	reader := &fakeSessionReader{reply: "Here is your report.\n[FILE:~/workspace/uploads/report.pdf]"}
	d := handler.NewDispatcher(gw, &fakeTokenProvider{}, sender, reader, "/data/workspace", 5*time.Second)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "send me the file", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	// Text part should update the placeholder.
	if sender.updates.Load() != 1 {
		t.Errorf("expected 1 UpdateMessage call, got %d", sender.updates.Load())
	}
	if sender.lastUpdatedText != "Here is your report." {
		t.Errorf("wrong updated text: %q", sender.lastUpdatedText)
	}
	// File should also be sent.
	if sender.filesSent.Load() != 1 {
		t.Errorf("expected 1 file sent, got %d", sender.filesSent.Load())
	}
	if sender.lastFilePath != "/data/workspace/uploads/report.pdf" {
		t.Errorf("wrong file path: %q", sender.lastFilePath)
	}
	// PostToChannel must not be called (update succeeded).
	if sender.sent.Load() != 0 {
		t.Errorf("PostToChannel must not be called when update succeeds, got %d", sender.sent.Load())
	}
}

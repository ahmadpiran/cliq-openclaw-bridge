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
}

func (f *fakeForwarder) Forward(_ context.Context, _ gateway.ForwardRequest) error {
	f.forwarded.Add(1)
	return f.forwardErr
}

func (f *fakeForwarder) DownloadAndForward(_ context.Context, _, _, _, _, _, _, _ string) error {
	f.fileForwarded.Add(1)
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
	err         error
	sent        atomic.Int32
	lastChannel string
	lastText    string
}

func (f *fakeZohoSender) PostToChannel(_ context.Context, channel, text string) error {
	f.sent.Add(1)
	f.lastChannel = channel
	f.lastText = text
	return f.err
}

type fakeSessionReader struct {
	reply    string
	replyErr error
	fileErr  error
	calls    atomic.Int32
}

func (f *fakeSessionReader) FindLatestSessionFile(_ time.Time) (string, error) {
	return "fake.jsonl", f.fileErr
}

func (f *fakeSessionReader) WaitForAssistantReply(_ context.Context, _ string, _ time.Time) (string, error) {
	f.calls.Add(1)
	return f.reply, f.replyErr
}

// --- helpers ---

func newDispatcher(
	gw handler.Forwarder,
	tp handler.TokenProvider,
	sender handler.ZohoSender,
	reader handler.SessionReader,
	replyTimeout time.Duration,
) *handler.Dispatcher {
	if replyTimeout == 0 {
		replyTimeout = 5 * time.Second
	}
	return handler.NewDispatcher(gw, tp, sender, reader, replyTimeout, "")
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
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, nil, 0)

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
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, nil, 0)

	job := worker.Job{RequestID: "req-raw", Payload: []byte(`NOT_JSON`), ReceivedAt: time.Now()}
	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gw.forwarded.Load() != 1 {
		t.Errorf("expected raw payload to be forwarded, got %d forwards", gw.forwarded.Load())
	}
}

func TestDispatcher_DownloadsAndForwardsFileAttachment(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{token: "zoho-tok-123"}
	d := handler.NewDispatcher(gw, tp, nil, nil, 5*time.Second, "/tmp/workspace")

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
	d := handler.NewDispatcher(gw, tp, nil, nil, 5*time.Second, "/tmp/workspace")

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

func TestDispatcher_PostsReplyToZohoCliqAfterAgentResponds(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{}
	reader := &fakeSessionReader{reply: "Hey Ross! 👋"}

	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader, 5*time.Second)

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
	if gw.forwarded.Load() != 1 {
		t.Errorf("expected 1 forward call, got %d", gw.forwarded.Load())
	}
	if sender.sent.Load() != 1 {
		t.Errorf("expected 1 reply sent, got %d", sender.sent.Load())
	}
	if sender.lastChannel != "CT_123" {
		t.Errorf("wrong channel: %s", sender.lastChannel)
	}
	if sender.lastText != "Hey Ross! 👋" {
		t.Errorf("wrong reply text: %s", sender.lastText)
	}
}

func TestDispatcher_NoReplyWhenSenderIsNil(t *testing.T) {
	gw := &fakeForwarder{}
	reader := &fakeSessionReader{reply: "some reply"}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil, reader, 5*time.Second)

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
	if reader.calls.Load() != 0 {
		t.Error("session reader must not be called when sender is nil")
	}
}

func TestDispatcher_NoReplyWhenChannelIsEmpty(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{}
	reader := &fakeSessionReader{reply: "some reply"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader, 5*time.Second)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"message_type": "text",
			// channel deliberately absent
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender.sent.Load() != 0 {
		t.Error("reply must not be sent when channel is empty")
	}
}

func TestDispatcher_ReplyBackSilentOnAgentTimeout(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{}
	reader := &fakeSessionReader{replyErr: context.DeadlineExceeded}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader, 5*time.Second)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"channel": "CT_123", "message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("agent timeout should not fail the job, got: %v", err)
	}
	if gw.forwarded.Load() != 1 {
		t.Errorf("expected 1 forward call regardless of reply timeout")
	}
	if sender.sent.Load() != 0 {
		t.Error("reply must not be sent when agent timed out")
	}
}

func TestDispatcher_ReplyBackSilentOnSenderFailure(t *testing.T) {
	gw := &fakeForwarder{}
	sender := &fakeZohoSender{err: errors.New("zoho api down")}
	reader := &fakeSessionReader{reply: "Hey!"}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender, reader, 5*time.Second)

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
	if sender.sent.Load() != 1 {
		t.Error("sender must have been called even though it errored")
	}
}

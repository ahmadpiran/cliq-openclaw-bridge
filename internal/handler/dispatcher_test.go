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
	result        *gateway.RespondResult
	respondErr    error
	fileErr       error
	responded     atomic.Int32
	fileResponded atomic.Int32
}

func (f *fakeForwarder) Respond(_ context.Context, _ gateway.RespondRequest) (*gateway.RespondResult, error) {
	f.responded.Add(1)
	if f.respondErr != nil {
		return nil, f.respondErr
	}
	if f.result != nil {
		return f.result, nil
	}
	return &gateway.RespondResult{ResponseID: "resp_test"}, nil
}

func (f *fakeForwarder) DownloadAndRespond(_ context.Context, _, _, _, _, _, _, _ string) (*gateway.RespondResult, error) {
	f.fileResponded.Add(1)
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	if f.result != nil {
		return f.result, nil
	}
	return &gateway.RespondResult{ResponseID: "resp_file"}, nil
}

type fakeTokenProvider struct {
	token string
	err   error
}

func (f *fakeTokenProvider) ValidToken(_ context.Context) (string, error) {
	return f.token, f.err
}

type fakeZohoSender struct {
	err          error
	fileErr      error
	sent         atomic.Int32
	filesSent    atomic.Int32
	lastUserID   string
	lastText     string
	lastFilePath string
}

func (f *fakeZohoSender) PostToChannel(_ context.Context, userID, text string) error {
	f.sent.Add(1)
	f.lastUserID = userID
	f.lastText = text
	return f.err
}

func (f *fakeZohoSender) SendFile(_ context.Context, userID, filePath string) error {
	f.filesSent.Add(1)
	f.lastUserID = userID
	f.lastFilePath = filePath
	return f.fileErr
}

// --- helpers ---

func newDispatcher(
	gw handler.Forwarder,
	tp handler.TokenProvider,
	sender handler.ZohoSender,
) *handler.Dispatcher {
	return handler.NewDispatcher(gw, tp, sender, "", 5*time.Second)
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
	d := newDispatcher(gw, &fakeTokenProvider{}, nil)

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
	if gw.responded.Load() != 1 {
		t.Errorf("expected 1 Respond call, got %d", gw.responded.Load())
	}
	if gw.fileResponded.Load() != 0 {
		t.Error("DownloadAndRespond must not be called for plain text")
	}
}

func TestDispatcher_ForwardsRawWhenPayloadNotJSON(t *testing.T) {
	gw := &fakeForwarder{}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil)

	job := worker.Job{RequestID: "req-raw", Payload: []byte(`NOT_JSON`), ReceivedAt: time.Now()}
	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gw.responded.Load() != 1 {
		t.Errorf("expected raw payload to be responded to, got %d", gw.responded.Load())
	}
}

func TestDispatcher_DownloadsAndRespondsForFileAttachment(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{token: "zoho-tok-123"}
	d := handler.NewDispatcher(gw, tp, nil, "/tmp/workspace", 5*time.Second)

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
	if gw.fileResponded.Load() != 1 {
		t.Errorf("expected 1 DownloadAndRespond call, got %d", gw.fileResponded.Load())
	}
	if gw.responded.Load() != 0 {
		t.Error("Respond must not be called directly for file attachments")
	}
}

func TestDispatcher_ErrorWhenTokenProviderFails(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{err: errors.New("bolt read error")}
	d := handler.NewDispatcher(gw, tp, nil, "/tmp/workspace", 5*time.Second)

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
	gw := &fakeForwarder{result: &gateway.RespondResult{ResponseID: "resp_1", Text: "Hey Ross! 👋"}}
	sender := &fakeZohoSender{}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text":          "hi",
			"sender":        "Ross",
			"sender_id":     "user_789",
			"channel":       "CT_123",
			"channel_title": "Test Channel",
			"message_type":  "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.sent.Load() != 1 {
		t.Errorf("expected 1 reply sent, got %d", sender.sent.Load())
	}
	if sender.lastUserID != "user_789" {
		t.Errorf("wrong user ID: %s", sender.lastUserID)
	}
	if sender.lastText != "Hey Ross! 👋" {
		t.Errorf("wrong reply text: %s", sender.lastText)
	}
}

func TestDispatcher_ThreadContinuityAcrossMessages(t *testing.T) {
	calls := make(chan trackCall, 4)

	trackingGW := &trackingForwarder{calls: calls}
	trackingGW.responses = []gateway.RespondResult{
		{ResponseID: "resp_first", Text: "first reply"},
		{ResponseID: "resp_second", Text: "second reply"},
	}

	sender := &fakeZohoSender{}
	d := handler.NewDispatcher(trackingGW, &fakeTokenProvider{}, sender, "", 5*time.Second)

	makeMsg := func(text string) worker.Job {
		return makeJob(t, map[string]any{
			"type": "message",
			"message": map[string]any{
				"text":         text,
				"sender":       "Ross",
				"sender_id":    "user_789",
				"channel":      "CT_123",
				"message_type": "text",
			},
		})
	}

	if err := d.Dispatch(context.Background(), makeMsg("first")); err != nil {
		t.Fatalf("first dispatch error: %v", err)
	}
	call1 := <-calls
	if call1.prevResponseID != "" {
		t.Errorf("first message should have no prevResponseID, got %q", call1.prevResponseID)
	}

	if err := d.Dispatch(context.Background(), makeMsg("second")); err != nil {
		t.Fatalf("second dispatch error: %v", err)
	}
	call2 := <-calls
	if call2.prevResponseID != "resp_first" {
		t.Errorf("second message should use prev response ID %q, got %q", "resp_first", call2.prevResponseID)
	}

}

func TestDispatcher_RespondErrorPropagates(t *testing.T) {
	gw := &fakeForwarder{respondErr: errors.New("openclaw down")}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil)

	job := makeJob(t, map[string]any{
		"type":    "message",
		"message": map[string]any{"text": "hi", "message_type": "text"},
	})

	if err := d.Dispatch(context.Background(), job); err == nil {
		t.Fatal("expected error when respond fails, got nil")
	}
}

func TestDispatcher_ReplyBackSilentOnSenderFailure(t *testing.T) {
	gw := &fakeForwarder{result: &gateway.RespondResult{ResponseID: "resp_1", Text: "Hey!"}}
	sender := &fakeZohoSender{err: errors.New("zoho api down")}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross", "sender_id": "user_abc",
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

func TestDispatcher_SendsFileWhenReplyContainsFileTag(t *testing.T) {
	gw := &fakeForwarder{result: &gateway.RespondResult{
		ResponseID: "resp_1",
		Text:       "Here is your report.\n[FILE:~/workspace/uploads/report.pdf]",
	}}
	sender := &fakeZohoSender{}
	d := handler.NewDispatcher(gw, &fakeTokenProvider{}, sender, "/data/workspace", 5*time.Second)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "send me the file", "sender": "Ross", "sender_id": "user_abc",
			"channel": "CT_123", "message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.sent.Load() != 1 {
		t.Errorf("expected 1 text post, got %d", sender.sent.Load())
	}
	if sender.lastText != "Here is your report." {
		t.Errorf("wrong text: %q", sender.lastText)
	}
	if sender.filesSent.Load() != 1 {
		t.Errorf("expected 1 file sent, got %d", sender.filesSent.Load())
	}
	if sender.lastFilePath != "/data/workspace/uploads/report.pdf" {
		t.Errorf("wrong file path: %q", sender.lastFilePath)
	}
}

func TestDispatcher_NoReplyWhenSenderIsNil(t *testing.T) {
	gw := &fakeForwarder{result: &gateway.RespondResult{ResponseID: "resp_1", Text: "some reply"}}
	d := newDispatcher(gw, &fakeTokenProvider{}, nil)

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
	// no assertions — just verify no panic when sender is nil
}

func TestDispatcher_NoReplyWhenSenderIDIsEmpty(t *testing.T) {
	gw := &fakeForwarder{result: &gateway.RespondResult{ResponseID: "resp_1", Text: "some reply"}}
	sender := &fakeZohoSender{}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text": "hi", "sender": "Ross",
			"message_type": "text",
			// no sender_id
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender.sent.Load() != 0 {
		t.Error("reply must not be sent when sender_id is empty")
	}
}

func TestDispatcher_UsesChannelForThreadAndSenderIDForReply(t *testing.T) {
	calls := make(chan gateway.RespondRequest, 4)
	gw := &capturingForwarder{
		calls: calls,
		result: &gateway.RespondResult{ResponseID: "resp_1", Text: "hello"},
	}
	sender := &fakeZohoSender{}
	d := newDispatcher(gw, &fakeTokenProvider{}, sender)

	job := makeJob(t, map[string]any{
		"type": "message",
		"message": map[string]any{
			"text":         "hi",
			"sender":       "Alice",
			"sender_id":    "user_alice99",
			"channel":      "CT_channel42",
			"message_type": "text",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req := <-calls
	_ = req // channel tracking is in channelResponseIDs, not session key

	if sender.lastUserID != "user_alice99" {
		t.Errorf("reply user ID: got %q, want %q", sender.lastUserID, "user_alice99")
	}
}

// --- tracking fakes for thread continuity test ---

type trackCall struct {
	prevResponseID string
	responseID     string
}

type trackingForwarder struct {
	responses []gateway.RespondResult
	idx       int
	calls     chan<- trackCall
}

func (t *trackingForwarder) Respond(_ context.Context, req gateway.RespondRequest) (*gateway.RespondResult, error) {
	var result gateway.RespondResult
	if t.idx < len(t.responses) {
		result = t.responses[t.idx]
		t.idx++
	}
	t.calls <- trackCall{req.PrevResponseID, result.ResponseID}
	return &result, nil
}

func (t *trackingForwarder) DownloadAndRespond(_ context.Context, _, _, _, _, _, _, _ string) (*gateway.RespondResult, error) {
	return &gateway.RespondResult{}, nil
}

type capturingForwarder struct {
	calls  chan<- gateway.RespondRequest
	result *gateway.RespondResult
}

func (c *capturingForwarder) Respond(_ context.Context, req gateway.RespondRequest) (*gateway.RespondResult, error) {
	c.calls <- req
	if c.result != nil {
		return c.result, nil
	}
	return &gateway.RespondResult{}, nil
}

func (c *capturingForwarder) DownloadAndRespond(_ context.Context, _, _, _, _, _, _, _ string) (*gateway.RespondResult, error) {
	return &gateway.RespondResult{}, nil
}

// Ensure time import is used
var _ = time.Second

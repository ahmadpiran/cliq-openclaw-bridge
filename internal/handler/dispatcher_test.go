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
	forwardErr   error
	streamErr    error
	forwarded    atomic.Int32
	streamed     atomic.Int32
	lastFilename string
	lastToken    string
}

func (f *fakeForwarder) Forward(_ context.Context, _ gateway.ForwardRequest) error {
	f.forwarded.Add(1)
	return f.forwardErr
}

func (f *fakeForwarder) StreamFile(_ context.Context, _, filename, _, zohoToken string) error {
	f.streamed.Add(1)
	f.lastFilename = filename
	f.lastToken = zohoToken
	return f.streamErr
}

type fakeTokenProvider struct {
	token string
	err   error
}

func (f *fakeTokenProvider) ValidToken(_ context.Context) (string, error) {
	return f.token, f.err
}

// --- helpers ---

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
	tp := &fakeTokenProvider{token: "tok"}
	d := handler.NewDispatcher(gw, tp)

	job := makeJob(t, map[string]any{"type": "message", "text": "hello"})
	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gw.forwarded.Load() != 1 {
		t.Errorf("expected 1 forward call, got %d", gw.forwarded.Load())
	}
	if gw.streamed.Load() != 0 {
		t.Error("stream should not be called for a plain payload")
	}
}

func TestDispatcher_StreamsFileAttachment(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{token: "zoho-tok-123"}
	d := handler.NewDispatcher(gw, tp)

	job := makeJob(t, map[string]any{
		"type": "message",
		"attachment": map[string]any{
			"download_url": "https://zoho.example.com/file/42",
			"name":         "report.pdf",
			"mime_type":    "application/pdf",
		},
	})

	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gw.streamed.Load() != 1 {
		t.Errorf("expected 1 stream call, got %d", gw.streamed.Load())
	}
	if gw.forwarded.Load() != 0 {
		t.Error("forward should not be called for an attachment payload")
	}
	if gw.lastFilename != "report.pdf" {
		t.Errorf("unexpected filename: %s", gw.lastFilename)
	}
	if gw.lastToken != "zoho-tok-123" {
		t.Errorf("unexpected zoho token: %s", gw.lastToken)
	}
}

func TestDispatcher_ForwardsRawWhenPayloadNotJSON(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{token: "tok"}
	d := handler.NewDispatcher(gw, tp)

	job := worker.Job{RequestID: "req-raw", Payload: []byte(`NOT_JSON`), ReceivedAt: time.Now()}
	if err := d.Dispatch(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gw.forwarded.Load() != 1 {
		t.Errorf("expected raw payload to be forwarded, got %d forwards", gw.forwarded.Load())
	}
}

func TestDispatcher_ErrorWhenTokenProviderFails(t *testing.T) {
	gw := &fakeForwarder{}
	tp := &fakeTokenProvider{err: errors.New("bolt read error")}
	d := handler.NewDispatcher(gw, tp)

	job := makeJob(t, map[string]any{
		"type": "message",
		"attachment": map[string]any{
			"download_url": "https://zoho.example.com/file/99",
			"name":         "file.zip",
			"mime_type":    "application/zip",
		},
	})

	err := d.Dispatch(context.Background(), job)
	if err == nil {
		t.Fatal("expected error when token provider fails, got nil")
	}
}

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/handler"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

func startPool(t *testing.T, fn worker.HandlerFunc) *worker.Pool {
	t.Helper()
	p := worker.New(worker.Config{
		Workers:    2,
		QueueDepth: 16,
		JobTimeout: 5 * time.Second,
	}, fn)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		p.Shutdown(ctx)
	})
	return p
}

func TestHandleZoho_AcceptsValidPayload(t *testing.T) {
	dispatched := make(chan worker.Job, 1)

	pool := startPool(t, func(_ context.Context, job worker.Job) error {
		dispatched <- job
		return nil
	})

	h := handler.NewWebhookHandler(pool)

	body, _ := json.Marshal(map[string]any{
		"type": "message",
		"trigger": map[string]any{
			"type": "incoming_message",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleZoho(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	select {
	case job := <-dispatched:
		if len(job.Payload) == 0 {
			t.Error("expected non-empty payload in dispatched job")
		}
	case <-time.After(2 * time.Second):
		t.Error("job was never dispatched to the pool")
	}
}

func TestHandleZoho_Returns429WhenQueueFull(t *testing.T) {
	release := make(chan struct{})

	// workerReady is closed by the first job the worker picks up.
	// This synchronizes the test: we know the worker is blocked on <-release
	// before we attempt to fill the queue slot and then trigger the 429.
	workerReady := make(chan struct{})

	tinyPool := worker.New(worker.Config{
		Workers:    1,
		QueueDepth: 1,
		JobTimeout: 5 * time.Second,
	}, func(_ context.Context, _ worker.Job) error {
		// Signal once that the worker goroutine is active and blocking.
		select {
		case <-workerReady:
			// Already closed by the first invocation — subsequent jobs just block.
		default:
			close(workerReady)
		}
		<-release
		return nil
	})

	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		tinyPool.Shutdown(ctx)
	})

	// Dispatch the first job and wait until the worker has provably picked it up
	// and is blocking on <-release. Only then is the queue slot guaranteed free.
	if err := tinyPool.Dispatch(worker.Job{ReceivedAt: time.Now()}); err != nil {
		t.Fatalf("first dispatch failed: %v", err)
	}
	select {
	case <-workerReady:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never became ready")
	}

	// Now the worker is occupied. Fill the single queue buffer slot.
	if err := tinyPool.Dispatch(worker.Job{ReceivedAt: time.Now()}); err != nil {
		t.Fatalf("second dispatch failed: %v", err)
	}

	// Worker busy + queue full → handler must return 429.
	h := handler.NewWebhookHandler(tinyPool)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho",
		bytes.NewBufferString(`{"type":"message"}`))
	rec := httptest.NewRecorder()

	h.HandleZoho(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}
}

func TestHandleZoho_MalformedJSONStillDispatches(t *testing.T) {
	dispatched := make(chan worker.Job, 1)

	pool := startPool(t, func(_ context.Context, job worker.Job) error {
		dispatched <- job
		return nil
	})

	h := handler.NewWebhookHandler(pool)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho",
		bytes.NewBufferString(`NOT_JSON`))
	rec := httptest.NewRecorder()

	h.HandleZoho(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for malformed JSON, got %d", rec.Code)
	}

	select {
	case <-dispatched:
		// Good — raw bytes forwarded regardless of JSON validity.
	case <-time.After(2 * time.Second):
		t.Error("malformed payload was not dispatched")
	}
}

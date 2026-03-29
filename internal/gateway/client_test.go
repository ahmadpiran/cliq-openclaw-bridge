package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
)

// newTestClient points the gateway client at a test server URL.
func newTestClient(t *testing.T, baseURL string) *gateway.Client {
	t.Helper()
	cfg := gateway.DefaultConfig(baseURL, "test-api-key")
	cfg.MaxRetries = 2
	cfg.InitialBackoff = 10 * time.Millisecond // Keep tests fast.
	cfg.MaxBackoff = 50 * time.Millisecond
	return gateway.New(cfg)
}

func TestForward_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			t.Errorf("missing or wrong auth header")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("could not decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.Forward(context.Background(), gateway.ForwardRequest{
		Source:     "zoho_cliq",
		RequestID:  "req-001",
		Payload:    json.RawMessage(`{"type":"message"}`),
		ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestForward_RetriesOn503(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.Forward(context.Background(), gateway.ForwardRequest{
		Source:    "zoho_cliq",
		RequestID: "req-retry",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Errorf("expected success after retries, got %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestForward_PermanentFailureOn400(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.Forward(context.Background(), gateway.ForwardRequest{
		Source:  "zoho_cliq",
		Payload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Error("expected error for 400, got nil")
	}
	// Must not retry on a permanent client error.
	if attempts.Load() != 1 {
		t.Errorf("expected exactly 1 attempt for 400, got %d", attempts.Load())
	}
}

func TestForward_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := client.Forward(ctx, gateway.ForwardRequest{
		Source:  "zoho_cliq",
		Payload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

func TestStreamFile_PipesDataWithoutBuffering(t *testing.T) {
	const fileContent = "hello from zoho file attachment"

	// Simulate Zoho file download endpoint.
	zohoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fileContent)
	}))
	defer zohoSrv.Close()

	var received string

	// Simulate OpenClaw upload endpoint.
	openclawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspace/files" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		received = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer openclawSrv.Close()

	client := newTestClient(t, openclawSrv.URL)

	// In TestStreamFile_PipesDataWithoutBuffering, update the call site:
	err := client.StreamFile(
		context.Background(),
		zohoSrv.URL+"/file/123",
		"report.pdf",
		"application/pdf",
		"", // zohoToken — test server does not enforce auth
	)
	if err != nil {
		t.Fatalf("StreamFile error: %v", err)
	}
	if !strings.Contains(received, fileContent) {
		t.Errorf("OpenClaw did not receive the file content; got: %q", received)
	}
}

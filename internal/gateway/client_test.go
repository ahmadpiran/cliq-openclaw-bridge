package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/gateway"
)

func newTestClient(t *testing.T, baseURL string) *gateway.Client {
	t.Helper()
	cfg := gateway.DefaultConfig(baseURL, "test-api-key")
	cfg.MaxRetries = 2
	cfg.InitialBackoff = 10 * time.Millisecond
	cfg.MaxBackoff = 50 * time.Millisecond
	return gateway.New(cfg)
}

func TestForward_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Confirm correct endpoint.
		if r.URL.Path != "/hooks/agent" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		// Confirm auth header.
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			t.Errorf("missing or wrong auth header")
		}
		// Confirm payload shape matches OpenClaw's expected format.
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("could not decode body: %v", err)
		}
		if _, ok := body["message"]; !ok {
			t.Error("payload missing required 'message' field")
		}
		if _, ok := body["name"]; !ok {
			t.Error("payload missing required 'name' field")
		}
		// Simulate OpenClaw's ack response.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"runId":"test-run-id"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.Forward(context.Background(), gateway.ForwardRequest{
		Message:    "[Zoho Cliq] Ross wrote in #general: hello",
		Name:       "Zoho Cliq",
		SessionKey: "hook:zoho-cliq",
		RequestID:  "req-001",
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
		w.Write([]byte(`{"ok":true,"runId":"retry-run-id"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.Forward(context.Background(), gateway.ForwardRequest{
		Message:    "retry test",
		Name:       "Zoho Cliq",
		SessionKey: "hook:zoho-cliq",
		RequestID:  "req-retry",
		ReceivedAt: time.Now(),
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
		Message:    "bad request test",
		Name:       "Zoho Cliq",
		ReceivedAt: time.Now(),
	})
	if err == nil {
		t.Error("expected error for 400, got nil")
	}
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
	cancel()

	err := client.Forward(ctx, gateway.ForwardRequest{
		Message:    "cancelled request",
		Name:       "Zoho Cliq",
		ReceivedAt: time.Now(),
	})
	if err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

func TestForward_InternalFieldsNotSerialised(t *testing.T) {
	// RequestID and ReceivedAt are internal — must not appear in the JSON body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("could not decode body: %v", err)
		}
		if _, ok := body["request_id"]; ok {
			t.Error("request_id must not be serialised into the OpenClaw payload")
		}
		if _, ok := body["received_at"]; ok {
			t.Error("received_at must not be serialised into the OpenClaw payload")
		}
		// sessionKey with omitempty — confirm it is present when set.
		if sk, ok := body["sessionKey"]; !ok || sk != "hook:zoho-cliq" {
			t.Errorf("expected sessionKey=hook:zoho-cliq, got %v", body["sessionKey"])
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"runId":"field-check-run"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.Forward(context.Background(), gateway.ForwardRequest{
		Message:    "field serialisation test",
		Name:       "Zoho Cliq",
		SessionKey: "hook:zoho-cliq",
		RequestID:  "should-not-appear",
		ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestForward_SessionKeyOmittedWhenEmpty(t *testing.T) {
	// sessionKey has omitempty — confirm it is absent when not set.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("could not decode body: %v", err)
		}
		if _, ok := body["sessionKey"]; ok {
			t.Error("sessionKey should be omitted when empty")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"runId":"omit-run"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.Forward(context.Background(), gateway.ForwardRequest{
		Message:    "omitempty test",
		Name:       "Zoho Cliq",
		ReceivedAt: time.Now(),
		// SessionKey deliberately left empty.
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDownloadAndRespond_SavesFileAndSendsMessage(t *testing.T) {
	const fileContent = "hello from zoho file attachment"

	// Simulate Zoho file download endpoint.
	zohoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fileContent)
	}))
	defer zohoSrv.Close()

	// Simulate OpenClaw /v1/responses endpoint (SSE stream).
	var receivedInput string
	openclawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		receivedInput, _ = body["input"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n"))
		w.Write([]byte("data: {\"type\":\"response.output_text.done\",\"text\":\"got it\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\"}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer openclawSrv.Close()

	workspaceDir := t.TempDir()
	client := newTestClient(t, openclawSrv.URL)

	result, err := client.DownloadAndRespond(
		context.Background(),
		zohoSrv.URL+"/file/123",
		"report.txt",
		"text/plain",
		"",              // zohoToken
		"please review", // comment
		"",              // prevResponseID (first message)
		workspaceDir,
	)
	if err != nil {
		t.Fatalf("DownloadAndRespond error: %v", err)
	}

	// Confirm the file was written to the workspace.
	savedPath := workspaceDir + "/uploads/report.txt"
	data, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("expected file to be saved at %s: %v", savedPath, err)
	}
	if string(data) != fileContent {
		t.Errorf("file content mismatch: got %q", string(data))
	}

	// Confirm the agent received a message referencing the file path.
	if !strings.Contains(receivedInput, "report.txt") {
		t.Errorf("agent input should reference filename, got: %q", receivedInput)
	}
	if !strings.Contains(receivedInput, "please review") {
		t.Errorf("agent input should include comment, got: %q", receivedInput)
	}

	if result.ResponseID != "resp_test" {
		t.Errorf("expected response ID %q, got %q", "resp_test", result.ResponseID)
	}
}

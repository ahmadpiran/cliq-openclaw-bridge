package session_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/session"
)

func TestWaitForAssistantReply(t *testing.T) {
	dir := t.TempDir()

	// Write a JSONL file that looks like an OpenClaw session.
	path := filepath.Join(dir, "test.jsonl")
	f, _ := os.Create(path)
	// User message (before our dispatch time — should be ignored)
	f.WriteString(`{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Zoho Cliq hi"}]}}` + "\n")
	f.Close()

	r := session.NewReader(dir, ".")

	// Before the assistant message is written, WaitForAssistantReply should block.
	afterTime := time.Now()

	// Write the assistant reply asynchronously after a short delay.
	go func() {
		time.Sleep(600 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
		f.WriteString(`{"type":"message","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","message":{"role":"assistant","content":[{"type":"text","text":"Hello from agent"}]}}` + "\n")
		f.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Override cached file directly for this test.
	reply, err := r.WaitForAssistantReply(ctx, path, afterTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "Hello from agent" {
		t.Errorf("expected 'Hello from agent', got %q", reply)
	}
}

func TestWaitForAssistantReply_Timeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(path, []byte{}, 0644)

	r := session.NewReader(dir, ".")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := r.WaitForAssistantReply(ctx, path, time.Now())
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

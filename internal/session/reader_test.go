package session_test

import (
	"context"
	"fmt"
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

func TestFindLatestSessionFile(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "main", "sessions")
	os.MkdirAll(sessDir, 0755)

	// Old file — written before dispatch
	old := filepath.Join(sessDir, "old.jsonl")
	os.WriteFile(old, []byte(`{"type":"message"}`+"\n"), 0644)

	before := time.Now()
	time.Sleep(10 * time.Millisecond)

	// New file — written after dispatch
	newFile := filepath.Join(sessDir, "new.jsonl")
	os.WriteFile(newFile, []byte(`{"type":"message"}`+"\n"), 0644)

	r := session.NewReader(dir, "main")
	got, err := r.FindLatestSessionFile(before)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != newFile {
		t.Errorf("expected %s, got %s", newFile, got)
	}
}

func TestTailAssistantMessages_ReceivesMultipleReplies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.jsonl")

	writeAssistant := func(text string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		entry := fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`,
			time.Now().UTC().Format(time.RFC3339Nano), text)
		f.WriteString(entry + "\n")
		f.Close()
	}

	writeAssistant("first reply")

	r := session.NewReader(dir, ".")
	out := make(chan string, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	afterTime := time.Now().Add(-1 * time.Second)

	go r.TailAssistantMessages(ctx, path, afterTime, 800*time.Millisecond, out)

	// Wait longer than one tick interval (500ms) so the first tick fires and
	// picks up "first reply" before "second reply" is written.
	// If both exist at the same tick, lastAssistantMessage returns only the last.
	time.Sleep(700 * time.Millisecond)
	writeAssistant("second reply after approval")

	var received []string
	timeout := time.After(4 * time.Second)
	for {
		select {
		case msg := <-out:
			received = append(received, msg)
			if len(received) == 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:
	if len(received) != 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(received), received)
	}
	if received[0] != "first reply" {
		t.Errorf("first message wrong: %s", received[0])
	}
	if received[1] != "second reply after approval" {
		t.Errorf("second message wrong: %s", received[1])
	}
}

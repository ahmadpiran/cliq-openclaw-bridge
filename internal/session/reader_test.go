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

func TestFindLatestSessionFile(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "main", "sessions")
	os.MkdirAll(sessDir, 0755)

	old := filepath.Join(sessDir, "old.jsonl")
	os.WriteFile(old, []byte(`{"type":"message"}`+"\n"), 0644)

	before := time.Now()
	time.Sleep(10 * time.Millisecond)

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
	path := filepath.Join(dir, "test.jsonl")

	writeAssistant := func(text string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		entry := fmt.Sprintf(
			`{"type":"message","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`,
			time.Now().UTC().Format(time.RFC3339Nano), text)
		f.WriteString(entry + "\n")
		f.Close()
	}

	writeAssistant("first reply")

	r := session.NewReader(dir, ".")
	out := make(chan string, 4)
	afterTime := time.Now().Add(-1 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go r.TailAssistantMessages(ctx, path, afterTime, out)

	// Wait longer than one tick (500ms) before writing second reply.
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
		t.Errorf("first: %s", received[0])
	}
	if received[1] != "second reply after approval" {
		t.Errorf("second: %s", received[1])
	}
}

func TestTailAssistantMessages_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(path, []byte{}, 0644)

	r := session.NewReader(dir, ".")
	out := make(chan string, 4)

	ctx, cancel := context.WithCancel(context.Background())
	go r.TailAssistantMessages(ctx, path, time.Now(), out)

	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(200 * time.Millisecond)

	// After cancel, no more sends should happen.
	select {
	case <-out:
		t.Error("received message after context cancel")
	default:
	}
}

package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// jsonlEntry is a single line in an OpenClaw session transcript file.
type jsonlEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message,omitempty"`
}

// Reader locates OpenClaw session JSONL files and extracts agent replies.
type Reader struct {
	agentsDir string
	agentID   string
}

// NewReader constructs a Reader.
func NewReader(agentsDir, agentID string) *Reader {
	return &Reader{agentsDir: agentsDir, agentID: agentID}
}

// FindLatestSessionFile returns the JSONL file most recently modified
// at or after afterTime. This reliably identifies which session file
// OpenClaw wrote to for the current dispatch — even when there are
// many session files from previous runs.
func (r *Reader) FindLatestSessionFile(afterTime time.Time) (string, error) {
	pattern := filepath.Join(r.agentsDir, r.agentID, "sessions", "*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("glob session files: %w", err)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no session files found in %s", pattern)
	}

	var best string
	var bestMod time.Time

	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		mod := info.ModTime()
		// Only consider files touched at or after the dispatch time.
		if mod.Before(afterTime) {
			continue
		}
		if mod.After(bestMod) {
			best = f
			bestMod = mod
		}
	}

	if best == "" {
		return "", fmt.Errorf("no session file modified after %s", afterTime.Format(time.RFC3339))
	}

	return best, nil
}

// WaitForAssistantReply polls sessionFile every 500ms until an assistant
// message written after afterTime appears, or the context is cancelled.
func (r *Reader) WaitForAssistantReply(ctx context.Context, sessionFile string, afterTime time.Time) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for agent reply: %w", ctx.Err())
		case <-ticker.C:
			text, err := lastAssistantMessage(sessionFile, afterTime)
			if err != nil || text == "" {
				continue
			}
			return text, nil
		}
	}
}

// lastAssistantMessage scans the JSONL file and returns the text of the
// most recent assistant message written after afterTime.
func lastAssistantMessage(path string, afterTime time.Time) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var result string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2<<20), 2<<20)

	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "message" || entry.Message == nil {
			continue
		}
		if entry.Message.Role != "assistant" {
			continue
		}
		if !entry.Timestamp.After(afterTime) {
			continue
		}
		for _, c := range entry.Message.Content {
			if c.Type == "text" && c.Text != "" {
				result = c.Text
			}
		}
	}

	return result, scanner.Err()
}

func (r *Reader) TailAssistantMessages(
	ctx context.Context,
	sessionFile string,
	afterTime time.Time,
	idleTimeout time.Duration,
	out chan<- string,
) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()

	lastSeen := afterTime

	for {
		select {
		case <-ctx.Done():
			return
		case <-idleTimer.C:
			return
		case <-ticker.C:
			text, err := lastAssistantMessage(sessionFile, lastSeen)
			if err != nil || text == "" {
				continue
			}
			// New message found — reset idle timer and advance the cursor.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(idleTimeout)
			lastSeen = time.Now()
			out <- text
		}
	}
}

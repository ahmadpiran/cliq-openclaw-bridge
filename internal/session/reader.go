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

func NewReader(agentsDir, agentID string) *Reader {
	return &Reader{agentsDir: agentsDir, agentID: agentID}
}

// FindLatestSessionFile returns the JSONL file most recently modified
// at or after afterTime.
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

// TailAssistantMessages watches sessionFile and sends each new assistant
// message to out as it appears. It stops only when ctx is cancelled —
// no idle timeout, so tool approvals and long-running tasks all come through.
func (r *Reader) TailAssistantMessages(
	ctx context.Context,
	sessionFile string,
	afterTime time.Time,
	out chan<- string,
) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastSeen := afterTime

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			text, err := lastAssistantMessage(sessionFile, lastSeen)
			if err != nil || text == "" {
				continue
			}
			lastSeen = time.Now()
			out <- text
		}
	}
}

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

package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// It is safe for concurrent use — all methods are read-only on disk.
type Reader struct {
	agentsDir string // e.g. /data/openclaw/agents
	agentID   string // e.g. "main"

	// cachedFile caches the resolved session file path after first discovery
	// so we don't scan all files on every request.
	cachedFile string
}

// NewReader constructs a Reader.
// agentsDir is the path to OpenClaw's agents directory inside the container.
func NewReader(agentsDir, agentID string) *Reader {
	return &Reader{agentsDir: agentsDir, agentID: agentID}
}

// SessionFile returns the JSONL file path for the hook:zoho-cliq session.
// On first call it scans all session files; subsequent calls use the cached path.
func (r *Reader) SessionFile() (string, error) {
	if r.cachedFile != "" {
		if _, err := os.Stat(r.cachedFile); err == nil {
			return r.cachedFile, nil
		}
		// Cached file no longer exists — clear and re-scan.
		r.cachedFile = ""
	}

	pattern := filepath.Join(r.agentsDir, r.agentID, "sessions", "*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("no session files found in %s", pattern)
	}

	// Find the most recently modified file that contains our hook name.
	var best string
	var bestMod time.Time
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		if info.ModTime().Before(bestMod) {
			continue
		}
		if fileContains(f, "Zoho Cliq") {
			best = f
			bestMod = info.ModTime()
		}
	}

	if best == "" {
		return "", fmt.Errorf("no session file found containing Zoho Cliq content — " +
			"send one message first so the session is created")
	}

	r.cachedFile = best
	return best, nil
}

// WaitForAssistantReply polls sessionFile every 500ms until an assistant message
// written after afterTime appears, or the context is cancelled.
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

// lastAssistantMessage scans the JSONL file and returns the text of the most
// recent assistant message written after afterTime. Returns ("", nil) when
// no qualifying message has been written yet.
func lastAssistantMessage(path string, afterTime time.Time) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var result string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2<<20), 2<<20) // 2 MiB — handles long context lines

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

// fileContains reports whether path contains substr on any line.
func fileContains(path, substr string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2<<20), 2<<20)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), substr) {
			return true
		}
	}
	return false
}

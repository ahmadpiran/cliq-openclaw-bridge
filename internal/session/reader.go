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

type jsonlEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"` // tool_use blocks
			ID   string `json:"id"`   // tool_use blocks
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

// FindLatestSessionFile returns the JSONL session file for the given session key.
//
// Strategy:
//  1. If sessionKey is non-empty, look for a file whose base name contains the
//     sanitised key — OpenClaw typically names session files after the session
//     key. This prevents tailing the wrong channel's file under concurrent load.
//  2. Fall back to the most recently modified file after afterTime (original
//     behaviour) in case OpenClaw uses opaque or timestamped filenames.
func (r *Reader) FindLatestSessionFile(afterTime time.Time, sessionKey string) (string, error) {
	pattern := filepath.Join(r.agentsDir, r.agentID, "sessions", "*.jsonl")
	all, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("glob session files: %w", err)
	}

	// Newer OpenClaw versions write <uuid>.trajectory.jsonl alongside the main
	// session file. Exclude them — they use a different internal format and
	// would shadow the correct session file when sorted by modification time.
	var files []string
	for _, f := range all {
		if !strings.HasSuffix(f, ".trajectory.jsonl") {
			files = append(files, f)
		}
	}

	if len(files) == 0 {
		return "", fmt.Errorf("no session files found in %s", pattern)
	}

	// Pass 1: prefer a file whose name contains the sanitised session key.
	// Only consider files modified at or after afterTime so we don't claim a
	// stale file from a previous conversation.
	if sessionKey != "" {
		safeKey := sanitiseSessionKey(sessionKey)
		for _, f := range files {
			if !strings.Contains(filepath.Base(f), safeKey) {
				continue
			}
			info, err := os.Stat(f)
			if err != nil {
				continue
			}
			if !info.ModTime().Before(afterTime) {
				return f, nil
			}
		}
	}

	// Pass 2: newest file by modification time — original fallback.
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

// sanitiseSessionKey replaces characters that are invalid or awkward in file
// names so the key can be matched against session file base names.
// e.g. "zoho-cliq:CT_abc123" → "zoho-cliq_CT_abc123"
func sanitiseSessionKey(key string) string {
	return strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(key)
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

// TailToolCalls watches sessionFile and sends each new tool call name to out
// as it appears. Tool calls are deduplicated by their ID. Stops when ctx is
// cancelled.
func (r *Reader) TailToolCalls(
	ctx context.Context,
	sessionFile string,
	afterTime time.Time,
	out chan<- string,
) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	seen := make(map[string]bool)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, name := range scanToolCalls(sessionFile, afterTime, seen) {
				select {
				case out <- name:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func scanToolCalls(path string, afterTime time.Time, seen map[string]bool) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var results []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2<<20), 2<<20)

	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "message" || entry.Message == nil || entry.Message.Role != "assistant" {
			continue
		}
		if !entry.Timestamp.After(afterTime) {
			continue
		}
		for _, c := range entry.Message.Content {
			if c.Type == "tool_use" && c.Name != "" && c.ID != "" && !seen[c.ID] {
				seen[c.ID] = true
				results = append(results, c.Name)
			}
		}
	}
	return results
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

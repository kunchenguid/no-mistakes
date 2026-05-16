package intent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeReaderName is the agent name used in cache keys and DB rows.
const ClaudeReaderName = "claude"

// claudeReader reads Claude Code transcripts from ~/.claude/projects/.
type claudeReader struct{}

// NewClaudeReader returns a Reader for Claude Code transcripts.
func NewClaudeReader() Reader { return &claudeReader{} }

func (r *claudeReader) Name() string { return ClaudeReaderName }

func claudeProjectDirName(cwd string) string {
	replacer := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return replacer.Replace(cwd)
}

func (r *claudeReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	home, err := resolveHome(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read claude projects: %w", err)
	}

	matcher := newRepoMatcher(ctx, opts.OriginCWD)
	var out []*Session

	for _, dir := range entries {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !dir.IsDir() {
			continue
		}
		dirPath := filepath.Join(root, dir.Name())
		files, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			modTime := info.ModTime()
			if !opts.WindowStart.IsZero() && modTime.Before(opts.WindowStart) {
				continue
			}
			if !opts.WindowEnd.IsZero() && modTime.After(opts.WindowEnd.Add(time.Hour)) {
				// Allow some slack on the end side too. Files modified far past
				// HeadTime are unlikely sources of this change.
				continue
			}
			path := filepath.Join(dirPath, f.Name())
			meta, err := claudePeekMetadata(path)
			if err != nil || meta == nil {
				continue
			}
			if !matcher.matches(ctx, meta.CWD) {
				continue
			}
			session := &Session{
				AgentName:    ClaudeReaderName,
				SessionID:    strings.TrimSuffix(f.Name(), ".jsonl"),
				CWD:          meta.CWD,
				StartedAt:    meta.FirstTimestamp,
				LastActivity: modTime,
				LastMsgKey:   modTime.UTC().Format(time.RFC3339Nano),
			}
			session.LastMsgKey = path + "|" + session.LastMsgKey
			session.startedAtPath = path
			out = append(out, session)
		}
	}
	return out, nil
}

func (r *claudeReader) Load(_ context.Context, s *Session) error {
	if s.startedAtPath == "" {
		return fmt.Errorf("claude: session has no path")
	}
	f, err := os.Open(s.startedAtPath)
	if err != nil {
		return fmt.Errorf("claude open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var lastUUID string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg, ok := parseClaudeRecord(line)
		if !ok {
			continue
		}
		if msg.uuid != "" {
			lastUUID = msg.uuid
		}
		if msg.message == nil {
			continue
		}
		s.Messages = append(s.Messages, *msg.message)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("claude scan: %w", err)
	}
	if lastUUID != "" {
		s.LastMsgKey = lastUUID
	}
	return nil
}

// claudeMetadata is the small subset returned by claudePeekMetadata.
type claudeMetadata struct {
	CWD            string
	FirstTimestamp time.Time
}

// claudePeekMetadata reads the first non-attachment record from a transcript
// file to extract its cwd and start time. Returns nil without an error when
// the file is empty or contains no parseable records.
func claudePeekMetadata(path string) (*claudeMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	var meta claudeMetadata
	for scanner.Scan() {
		var raw struct {
			CWD       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		if raw.CWD == "" {
			continue
		}
		meta.CWD = raw.CWD
		if raw.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, raw.Timestamp); err == nil {
				meta.FirstTimestamp = t
			} else if t, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil {
				meta.FirstTimestamp = t
			}
		}
		return &meta, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

// claudeRecord is the parsed shape of one .jsonl line we care about.
type claudeRecord struct {
	uuid    string
	message *Message
}

// parseClaudeRecord returns a Message for user and assistant turns. It
// returns ok=true with message=nil for records we want to track for
// LastMsgKey/uuid purposes but should not include in Messages.
func parseClaudeRecord(line []byte) (claudeRecord, bool) {
	var raw struct {
		Type      string          `json:"type"`
		UUID      string          `json:"uuid"`
		Timestamp string          `json:"timestamp"`
		IsMeta    bool            `json:"isMeta"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return claudeRecord{}, false
	}

	rec := claudeRecord{uuid: raw.UUID}
	ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)

	switch raw.Type {
	case "user":
		if raw.IsMeta {
			return rec, true
		}
		text, paths := parseClaudeUserMessage(raw.Message)
		text = strings.TrimSpace(text)
		if text == "" {
			return rec, true
		}
		if isClaudeSyntheticUserText(text) {
			return rec, true
		}
		rec.message = &Message{
			Role:      RoleUser,
			Text:      text,
			FilePaths: paths,
			Timestamp: ts,
		}
	case "assistant":
		text, paths := parseClaudeAssistantMessage(raw.Message)
		text = strings.TrimSpace(text)
		if text == "" && len(paths) == 0 {
			return rec, true
		}
		rec.message = &Message{
			Role:      RoleAssistant,
			Text:      text,
			FilePaths: paths,
			Timestamp: ts,
		}
	default:
		return rec, true
	}
	return rec, true
}

// parseClaudeUserMessage extracts text and tool_result file paths from a
// user record. content may be a plain string or an array of typed items.
func parseClaudeUserMessage(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", nil
	}
	if len(msg.Content) == 0 {
		return "", nil
	}
	// String form.
	if msg.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(msg.Content, &s); err == nil {
			return s, nil
		}
	}
	// Array form: each item may be {type: "text", text: "..."} or
	// {type: "tool_result", content: ...}. We drop tool_result *text*
	// entirely - it's tool output, not user intent.
	var items []map[string]any
	if err := json.Unmarshal(msg.Content, &items); err != nil {
		return "", nil
	}
	var sb strings.Builder
	for _, item := range items {
		if t, _ := item["type"].(string); t == "text" {
			if s, ok := item["text"].(string); ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String(), nil
}

// parseClaudeAssistantMessage extracts assistant text and any file paths
// referenced via tool_use input fields. Thinking blocks are dropped.
func parseClaudeAssistantMessage(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var msg struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", nil
	}
	var sb strings.Builder
	var paths []string
	for _, item := range msg.Content {
		t, _ := item["type"].(string)
		switch t {
		case "text":
			if s, ok := item["text"].(string); ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		case "tool_use":
			if input, ok := item["input"].(map[string]any); ok {
				paths = append(paths, extractToolPaths(input)...)
			}
		}
	}
	return sb.String(), paths
}

// extractToolPaths pulls plausible file paths from tool input fields.
// Agent tools use several key names for path-like values; cover the common
// variants here so transcript readers can share the same extraction logic.
func extractToolPaths(input map[string]any) []string {
	var out []string
	for _, key := range []string{"file_path", "filePath", "path", "notebook_path"} {
		if s, ok := input[key].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if pattern, ok := input["pattern"].(string); ok {
		// Patterns may contain globs; still useful as a hint.
		out = append(out, pattern)
	}
	if edits, ok := input["edits"].([]any); ok {
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				if s, ok := m["file_path"].(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// isClaudeSyntheticUserText filters out the meta strings the Claude CLI
// inserts as fake "user" messages: slash-command echoes, caveats, etc.
func isClaudeSyntheticUserText(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "<command-name>") {
		return true
	}
	if strings.HasPrefix(t, "<local-command-caveat>") {
		return true
	}
	if strings.HasPrefix(t, "Caveat:") {
		return true
	}
	return false
}

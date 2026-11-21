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

// OMPReaderName is the agent name used in cache keys and DB rows.
const OMPReaderName = "omp"

const ompScannerMaxTokenSize = 256 * 1024 * 1024

// ompReader reads OMP coding-agent transcripts from ~/.omp/agent/sessions/.
type ompReader struct{}

// NewOMPReader returns a Reader for OMP coding-agent transcripts.
func NewOMPReader() Reader { return &ompReader{} }

func (r *ompReader) Name() string { return OMPReaderName }

func (r *ompReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	homeDir := opts.HomeDir
	if homeDir == "" && opts.TranscriptHomes != nil && opts.TranscriptHomes.OMPHome != "" {
		homeDir = opts.TranscriptHomes.OMPHome
	}
	home, err := resolveHome(homeDir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".omp", "agent", "sessions")
	repoDirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("omp sessions: %w", err)
	}

	matcher := newRepoMatcher(ctx, opts.OriginCWD)
	var out []*Session
	for _, repoDir := range repoDirs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !repoDir.IsDir() {
			continue
		}
		dirPath := filepath.Join(root, repoDir.Name())
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
				continue
			}
			path := filepath.Join(dirPath, f.Name())
			meta, err := ompPeekMetadata(path)
			if err != nil || meta == nil {
				continue
			}
			if !matcher.matches(ctx, meta.cwd) {
				continue
			}
			sessionID := meta.id
			if sessionID == "" {
				sessionID = strings.TrimSuffix(f.Name(), ".jsonl")
			}
			session := &Session{
				AgentName:     OMPReaderName,
				SessionID:     sessionID,
				CWD:           meta.cwd,
				StartedAt:     meta.startedAt,
				LastActivity:  modTime,
				LastMsgKey:    modTime.UTC().Format(time.RFC3339Nano),
				startedAtPath: path,
			}
			session.LastMsgKey = path + "|" + session.LastMsgKey
			out = append(out, session)
		}
	}
	return out, nil
}

func (r *ompReader) Load(_ context.Context, s *Session) error {
	if s.startedAtPath == "" {
		return fmt.Errorf("omp: session has no path")
	}
	f, err := os.Open(s.startedAtPath)
	if err != nil {
		return fmt.Errorf("omp open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), ompScannerMaxTokenSize)
	var lastID string
	seen := make(map[string]struct{})
	seenLive := make(map[string]struct{})
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msgs, id, aggregate, ok := parseOMPRecord(line)
		if !ok {
			continue
		}
		if id != "" {
			lastID = id
		}
		priorSeen := seen
		if aggregate {
			priorSeen = make(map[string]struct{}, len(seen))
			for key := range seen {
				priorSeen[key] = struct{}{}
			}
		}
		for _, msg := range msgs {
			if !aggregate && msg.identity != "" {
				if _, ok := seenLive[msg.identity]; ok {
					continue
				}
				seenLive[msg.identity] = struct{}{}
			}
			key := ompMessageKey(msg.Message)
			if aggregate {
				if _, ok := priorSeen[key]; ok {
					continue
				}
			}
			seen[key] = struct{}{}
			s.Messages = append(s.Messages, msg.Message)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("omp scan: %w", err)
	}
	if lastID != "" {
		s.LastMsgKey = lastID
	}
	return nil
}

func ompMessageKey(msg Message) string {
	return string(msg.Role) + "\x00" + msg.Text + "\x00" + strings.Join(msg.FilePaths, "\x00")
}

type ompParsedMessage struct {
	Message
	identity string
}

type ompMetadata struct {
	id        string
	cwd       string
	startedAt time.Time
}

func ompPeekMetadata(path string) (*ompMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), ompScannerMaxTokenSize)
	for scanner.Scan() {
		var raw struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
			CWD       string `json:"cwd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		if raw.Type != "session" || raw.CWD == "" {
			continue
		}
		startedAt, _ := parseOMPTimestamp(raw.Timestamp)
		return &ompMetadata{id: raw.ID, cwd: raw.CWD, startedAt: startedAt}, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

func parseOMPRecord(line []byte) ([]ompParsedMessage, string, bool, bool) {
	var raw struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
		Messages  json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, "", false, false
	}
	switch raw.Type {
	case "message", "message_end", "turn_end":
		msg, ok := parseOMPParsedMessage(raw.Message, raw.Timestamp)
		if !ok {
			return nil, raw.ID, false, true
		}
		return []ompParsedMessage{msg}, raw.ID, false, true
	case "message_update":
		return nil, raw.ID, false, true
	case "agent_end":
		return parseOMPMessages(raw.Messages), raw.ID, true, true
	default:
		return nil, raw.ID, false, true
	}
}

func parseOMPMessages(raw json.RawMessage) []ompParsedMessage {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	msgs := make([]ompParsedMessage, 0, len(items))
	for _, item := range items {
		msg, ok := parseOMPParsedMessage(item, "")
		if ok {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

func parseOMPMessage(raw json.RawMessage, timestamp string) (Message, bool) {
	msg, ok := parseOMPParsedMessage(raw, timestamp)
	return msg.Message, ok
}

func parseOMPParsedMessage(raw json.RawMessage, timestamp string) (ompParsedMessage, bool) {
	if len(raw) == 0 {
		return ompParsedMessage{}, false
	}

	var msg struct {
		Role       string          `json:"role"`
		ID         string          `json:"id"`
		ResponseID string          `json:"responseId"`
		Content    json.RawMessage `json:"content"`
		Timestamp  json.RawMessage `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ompParsedMessage{}, false
	}

	role := RoleAssistant
	switch {
	case strings.EqualFold(msg.Role, "user"):
		role = RoleUser
	case strings.EqualFold(msg.Role, "assistant"):
		role = RoleAssistant
	default:
		return ompParsedMessage{}, false
	}

	text, paths := parseOMPContent(msg.Content)
	text = strings.TrimSpace(text)
	if role == RoleUser {
		paths = append(paths, scanFilePathsInText(text)...)
	}
	if text == "" && len(paths) == 0 {
		return ompParsedMessage{}, false
	}
	ts, _ := parseOMPTimestamp(timestamp)
	if len(msg.Timestamp) > 0 && string(msg.Timestamp) != "null" {
		var millis int64
		var text string
		if err := json.Unmarshal(msg.Timestamp, &millis); err == nil {
			ts = time.UnixMilli(millis)
		} else if err := json.Unmarshal(msg.Timestamp, &text); err == nil {
			ts, _ = parseOMPTimestamp(text)
		}
	}
	out := Message{
		Role:      role,
		Text:      text,
		FilePaths: paths,
		Timestamp: ts,
	}
	identity := ""
	if msg.ResponseID != "" {
		identity = string(role) + "\x00responseId:" + msg.ResponseID
	} else if msg.ID != "" {
		identity = string(role) + "\x00id:" + msg.ID
	}
	return ompParsedMessage{Message: out, identity: identity}, true
}

func parseOMPContent(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s, nil
		}
		return "", nil
	}

	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", nil
	}
	var sb strings.Builder
	var paths []string
	for _, item := range items {
		t, _ := item["type"].(string)
		switch t {
		case "text", "input_text", "output_text":
			if s, ok := item["text"].(string); ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			} else if s, ok := item["content"].(string); ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		case "toolCall", "tool_call", "tool_use":
			paths = append(paths, ompToolCallPaths(item)...)
		}
	}
	return sb.String(), paths
}

// extractToolPaths pulls plausible file paths from tool input fields.
func extractToolPaths(input map[string]any) []string {
	var out []string
	for _, key := range []string{"file_path", "filePath", "path", "notebook_path"} {
		if s, ok := input[key].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if pattern, ok := input["pattern"].(string); ok {
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

func ompToolCallPaths(item map[string]any) []string {
	var out []string
	for _, key := range []string{"arguments", "input"} {
		args, ok := item[key]
		if !ok {
			continue
		}
		switch v := args.(type) {
		case map[string]any:
			out = append(out, extractToolPaths(v)...)
			out = append(out, ompCommandPaths(v["command"])...)
		case string:
			var m map[string]any
			if err := json.Unmarshal([]byte(v), &m); err == nil {
				out = append(out, extractToolPaths(m)...)
				out = append(out, ompCommandPaths(m["command"])...)
			} else {
				out = append(out, scanFilePathsInText(v)...)
			}
		}
	}
	return out
}

func ompCommandPaths(raw any) []string {
	switch v := raw.(type) {
	case string:
		return scanFilePathsInText(v)
	case []any:
		var out []string
		for _, part := range v {
			if s, ok := part.(string); ok {
				out = append(out, scanFilePathsInText(s)...)
			}
		}
		return out
	default:
		return nil
	}
}

func parseOMPTimestamp(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}

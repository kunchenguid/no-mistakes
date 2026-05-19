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

// PiReaderName is the agent name used in cache keys and DB rows.
const PiReaderName = "pi"

const piScannerMaxTokenSize = 256 * 1024 * 1024

// piReader reads Pi coding-agent transcripts from ~/.pi/agent/sessions/.
type piReader struct{}

// NewPiReader returns a Reader for Pi coding-agent transcripts.
func NewPiReader() Reader { return &piReader{} }

func (r *piReader) Name() string { return PiReaderName }

func (r *piReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	home, err := resolveHome(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".pi", "agent", "sessions")
	repoDirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("pi sessions: %w", err)
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
			meta, err := piPeekMetadata(path)
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
				AgentName:     PiReaderName,
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

func (r *piReader) Load(_ context.Context, s *Session) error {
	if s.startedAtPath == "" {
		return fmt.Errorf("pi: session has no path")
	}
	f, err := os.Open(s.startedAtPath)
	if err != nil {
		return fmt.Errorf("pi open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), piScannerMaxTokenSize)
	var lastID string
	seen := make(map[string]struct{})
	seenLive := make(map[string]struct{})
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msgs, id, aggregate, ok := parsePiRecord(line)
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
			key := piMessageKey(msg.Message)
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
		return fmt.Errorf("pi scan: %w", err)
	}
	if lastID != "" {
		s.LastMsgKey = lastID
	}
	return nil
}

func piMessageKey(msg Message) string {
	return string(msg.Role) + "\x00" + msg.Text + "\x00" + strings.Join(msg.FilePaths, "\x00")
}

type piParsedMessage struct {
	Message
	identity string
}

type piMetadata struct {
	id        string
	cwd       string
	startedAt time.Time
}

func piPeekMetadata(path string) (*piMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), piScannerMaxTokenSize)
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
		startedAt, _ := parsePiTimestamp(raw.Timestamp)
		return &piMetadata{id: raw.ID, cwd: raw.CWD, startedAt: startedAt}, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

func parsePiRecord(line []byte) ([]piParsedMessage, string, bool, bool) {
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
		msg, ok := parsePiParsedMessage(raw.Message, raw.Timestamp)
		if !ok {
			return nil, raw.ID, false, true
		}
		return []piParsedMessage{msg}, raw.ID, false, true
	case "message_update":
		return nil, raw.ID, false, true
	case "agent_end":
		return parsePiMessages(raw.Messages, raw.Timestamp), raw.ID, true, true
	default:
		return nil, raw.ID, false, true
	}
}

func parsePiMessages(raw json.RawMessage, timestamp string) []piParsedMessage {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	msgs := make([]piParsedMessage, 0, len(items))
	for _, item := range items {
		msg, ok := parsePiParsedMessage(item, timestamp)
		if ok {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

func parsePiMessage(raw json.RawMessage, timestamp string) (Message, bool) {
	msg, ok := parsePiParsedMessage(raw, timestamp)
	return msg.Message, ok
}

func parsePiParsedMessage(raw json.RawMessage, timestamp string) (piParsedMessage, bool) {
	if len(raw) == 0 {
		return piParsedMessage{}, false
	}

	var msg struct {
		Role       string          `json:"role"`
		ID         string          `json:"id"`
		ResponseID string          `json:"responseId"`
		Content    json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return piParsedMessage{}, false
	}

	role := RoleAssistant
	switch {
	case strings.EqualFold(msg.Role, "user"):
		role = RoleUser
	case strings.EqualFold(msg.Role, "assistant"):
		role = RoleAssistant
	default:
		return piParsedMessage{}, false
	}

	text, paths := parsePiContent(msg.Content)
	text = strings.TrimSpace(text)
	if role == RoleUser {
		paths = append(paths, scanFilePathsInText(text)...)
	}
	if text == "" && len(paths) == 0 {
		return piParsedMessage{}, false
	}
	ts, _ := parsePiTimestamp(timestamp)
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
	return piParsedMessage{Message: out, identity: identity}, true
}

func parsePiContent(raw json.RawMessage) (string, []string) {
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
			paths = append(paths, piToolCallPaths(item)...)
		}
	}
	return sb.String(), paths
}

func piToolCallPaths(item map[string]any) []string {
	var out []string
	for _, key := range []string{"arguments", "input"} {
		args, ok := item[key]
		if !ok {
			continue
		}
		switch v := args.(type) {
		case map[string]any:
			out = append(out, extractToolPaths(v)...)
			out = append(out, piCommandPaths(v["command"])...)
		case string:
			var m map[string]any
			if err := json.Unmarshal([]byte(v), &m); err == nil {
				out = append(out, extractToolPaths(m)...)
				out = append(out, piCommandPaths(m["command"])...)
			} else {
				out = append(out, scanFilePathsInText(v)...)
			}
		}
	}
	return out
}

func piCommandPaths(raw any) []string {
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

func parsePiTimestamp(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}

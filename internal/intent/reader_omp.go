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

// ompReader reads omp coding-agent transcripts from ~/.omp/agent/sessions/.
type ompReader struct{}

// NewOMPReader returns a Reader for omp coding-agent transcripts.
func NewOMPReader() Reader { return &ompReader{} }

func (r *ompReader) Name() string { return OMPReaderName }

func (r *ompReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	home, err := resolveHome(opts.HomeDir)
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
	seen := make(map[string]struct{})
	seenLive := make(map[string]struct{})
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msgs, aggregate, ok := parseOMPRecord(line)
		if !ok {
			continue
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
	return scanner.Err()
}

// ompParsedMessage wraps a Message with a deduplication identity.
type ompParsedMessage struct {
	Message
	identity string
}

// ompSessionMeta holds the metadata peeked from the first line of a session file.
type ompSessionMeta struct {
	id        string
	cwd       string
	startedAt time.Time
}

func ompPeekMetadata(path string) (*ompSessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), ompScannerMaxTokenSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		meta := &ompSessionMeta{}
		if v, ok := raw["sessionId"].(string); ok {
			meta.id = v
		}
		if v, ok := raw["cwd"].(string); ok {
			meta.cwd = v
		}
		if v, ok := raw["directory"].(string); ok && meta.cwd == "" {
			meta.cwd = v
		}
		if v, ok := raw["startedAt"].(string); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				meta.startedAt = t
			}
		}
		return meta, nil
	}
	return nil, scanner.Err()
}

// parseOMPRecord parses one JSONL line from an omp session file.
// omp session format uses the same event types as Pi: message_update,
// message_end, turn_end, agent_end.
func parseOMPRecord(line []byte) ([]ompParsedMessage, bool, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, false, false
	}

	typ, _ := raw["type"].(string)
	switch typ {
	case "message_update", "message_end", "turn_end":
		msg, ok := raw["message"].(map[string]any)
		if !ok {
			return nil, false, false
		}
		role, _ := msg["role"].(string)
		if role != "assistant" && role != "user" {
			return nil, false, false
		}
		text := ompExtractText(msg)
		if text == "" {
			return nil, false, false
		}
		id, _ := msg["responseId"].(string)
		pm := ompParsedMessage{
			Message: Message{
				Role: Role(role),
				Text: text,
			},
			identity: id,
		}
		return []ompParsedMessage{pm}, false, true

	case "agent_end":
		msgs, ok := raw["messages"].([]any)
		if !ok {
			return nil, false, false
		}
		var out []ompParsedMessage
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role != "assistant" && role != "user" {
				continue
			}
			text := ompExtractText(msg)
			if text == "" {
				continue
			}
			id, _ := msg["responseId"].(string)
			out = append(out, ompParsedMessage{
				Message: Message{
					Role: Role(role),
					Text: text,
				},
				identity: id,
			})
		}
		return out, true, true
	}
	return nil, false, false
}

// ompExtractText extracts text content from an omp message object.
func ompExtractText(msg map[string]any) string {
	if text, ok := msg["content"].(string); ok && text != "" {
		return text
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func ompMessageKey(msg Message) string {
	return string(msg.Role) + ":" + msg.Text
}

package intent

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// CodexReaderName is the agent name used in cache keys and DB rows.
const CodexReaderName = "codex"

// codexReader reads Codex CLI sessions. Session metadata (cwd, timestamps,
// rollout path) lives in ~/.codex/state_*.sqlite; the actual transcript is
// a JSONL rollout file referenced by threads.rollout_path. We use the
// SQLite to filter candidates fast, then parse the rollout to recover the
// full user/assistant turn-by-turn text needed for intent inference.
type codexReader struct{}

// NewCodexReader returns a Reader for Codex CLI transcripts.
func NewCodexReader() Reader { return &codexReader{} }

func (r *codexReader) Name() string { return CodexReaderName }

func (r *codexReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	home, err := resolveHome(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	codexHome := filepath.Join(home, ".codex")
	dbPath, err := resolveCodexStateDB(codexHome)
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		return nil, nil
	}

	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, fmt.Errorf("codex open: %w", err)
	}
	defer db.Close()

	matcher := newRepoMatcher(ctx, opts.OriginCWD)
	winStart := opts.WindowStart.Unix()
	winEnd := opts.WindowEnd.Add(time.Hour).Unix()

	rows, err := db.QueryContext(ctx,
		`SELECT id, cwd, created_at, updated_at, rollout_path
		 FROM threads
		 WHERE updated_at >= ? AND created_at <= ?
		 ORDER BY updated_at DESC
		 LIMIT 200`,
		winStart, winEnd)
	if err != nil {
		// threads table missing or schema changed: treat as no data.
		return nil, nil
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		var (
			id, cwd, rolloutPath string
			createdAt, updatedAt int64
		)
		if err := rows.Scan(&id, &cwd, &createdAt, &updatedAt, &rolloutPath); err != nil {
			continue
		}
		if !matcher.matches(ctx, cwd) {
			continue
		}
		// Resolve relative rollout paths against ~/.codex.
		if rolloutPath != "" && !filepath.IsAbs(rolloutPath) {
			rolloutPath = filepath.Join(codexHome, rolloutPath)
		}
		out = append(out, &Session{
			AgentName:     CodexReaderName,
			SessionID:     id,
			CWD:           cwd,
			StartedAt:     time.Unix(createdAt, 0).UTC(),
			LastActivity:  time.Unix(updatedAt, 0).UTC(),
			LastMsgKey:    fmt.Sprintf("%d", updatedAt),
			startedAtPath: rolloutPath,
		})
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func (r *codexReader) Load(_ context.Context, s *Session) error {
	if s.startedAtPath == "" {
		// No rollout file. Without per-turn text, this session can't
		// contribute meaningful intent; skip rather than fabricate.
		return fmt.Errorf("codex: session has no rollout path")
	}
	f, err := os.Open(s.startedAtPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("codex rollout missing: %s", s.startedAtPath)
		}
		return fmt.Errorf("codex open rollout: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg, ok := parseCodexLine(line)
		if !ok {
			continue
		}
		s.Messages = append(s.Messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("codex rollout scan: %w", err)
	}
	return nil
}

// parseCodexLine returns a Message for the user/assistant turns we care
// about. Tool calls produce file-path hints attached to a file-path-only
// Message so the matcher can use them; their arguments do NOT enter
// Message.Text since that would leak shell commands and tool I/O into
// the summarizer's input.
func parseCodexLine(line []byte) (Message, bool) {
	var raw struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return Message{}, false
	}

	switch raw.Type {
	case "event_msg":
		return parseCodexEventMsg(raw.Payload)
	case "response_item":
		return parseCodexResponseItem(raw.Payload)
	default:
		return Message{}, false
	}
}

func parseCodexEventMsg(payload json.RawMessage) (Message, bool) {
	var ev struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return Message{}, false
	}
	if ev.Type != "user_message" {
		return Message{}, false
	}
	text := strings.TrimSpace(ev.Message)
	if text == "" {
		return Message{}, false
	}
	return Message{
		Role:      RoleUser,
		Text:      text,
		FilePaths: scanFilePathsInText(text),
	}, true
}

func parseCodexResponseItem(payload json.RawMessage) (Message, bool) {
	var item struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
	}
	if err := json.Unmarshal(payload, &item); err != nil {
		return Message{}, false
	}

	switch item.Type {
	case "message":
		// Assistant or user. user_message envelope above is the
		// canonical user-turn shape, but some recorders emit user
		// content under response_item too.
		role := RoleAssistant
		if strings.EqualFold(item.Role, "user") {
			role = RoleUser
		}
		text := codexJoinContent(item.Content)
		text = strings.TrimSpace(text)
		if text == "" {
			return Message{}, false
		}
		return Message{
			Role:      role,
			Text:      text,
			FilePaths: scanFilePathsInText(text),
		}, true
	case "function_call":
		// Tool call: capture file paths from the arguments JSON for
		// matching, but keep Text empty. Attach to an assistant message
		// because tool calls are made by the assistant turn.
		paths := codexExtractToolPaths(item.Name, item.Arguments)
		if len(paths) == 0 {
			return Message{}, false
		}
		return Message{
			Role:      RoleAssistant,
			FilePaths: paths,
		}, true
	default:
		return Message{}, false
	}
}

// codexJoinContent flattens a content array into a single string. The schema
// allows `[{"type":"output_text","text":"..."}]` for assistant text and
// `[{"type":"input_text","text":"..."}]` for user text. We accept both.
func codexJoinContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// String form (rare but tolerated by some Codex versions).
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, item := range items {
		t, _ := item["type"].(string)
		switch t {
		case "output_text", "input_text", "text":
			if s, ok := item["text"].(string); ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// codexExtractToolPaths pulls file paths out of a tool call's arguments
// JSON. Codex's main tools are `shell` (a command list) and read/write
// helpers that pass file_path/path keys. We handle both shapes so the
// matcher gets useful hints without needing Codex-specific knowledge.
func codexExtractToolPaths(toolName, argumentsJSON string) []string {
	if argumentsJSON == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
		// Some shells double-encode; try one level of unwrap.
		var asString string
		if err := json.Unmarshal([]byte(argumentsJSON), &asString); err == nil {
			return scanFilePathsInText(asString)
		}
		return nil
	}

	out := extractToolPaths(args)

	// shell tool: arguments contain a "command" array of strings.
	if cmdAny, ok := args["command"]; ok {
		switch cmd := cmdAny.(type) {
		case string:
			out = append(out, scanFilePathsInText(cmd)...)
		case []any:
			for _, part := range cmd {
				if s, ok := part.(string); ok {
					out = append(out, scanFilePathsInText(s)...)
				}
			}
		}
	}
	return out
}

// resolveCodexStateDB picks the highest-numbered state_<N>.sqlite under root.
// Codex versions its state DB; we want the most recent one without hard-coding
// the suffix. Sort numerically by the <N> suffix - lexicographic order would
// rank state_9 above state_10 once Codex reaches two-digit versions.
// Files whose suffix doesn't parse as an integer are placed at the end so
// they never override a real numbered DB.
func resolveCodexStateDB(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var candidates []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "state_") || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		candidates = append(candidates, name)
	}
	if len(candidates) == 0 {
		return "", nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		ni, oki := codexStateVersion(candidates[i])
		nj, okj := codexStateVersion(candidates[j])
		switch {
		case oki && okj:
			return ni > nj
		case oki:
			return true
		case okj:
			return false
		default:
			return candidates[i] > candidates[j]
		}
	})
	return filepath.Join(root, candidates[0]), nil
}

// codexStateVersion extracts the integer N from "state_N.sqlite". Returns
// (0, false) when the suffix is missing or non-numeric.
func codexStateVersion(name string) (int, bool) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "state_"), ".sqlite")
	if trimmed == "" {
		return 0, false
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, false
	}
	return n, true
}

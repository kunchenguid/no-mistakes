package intent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GrokReaderName is the agent name used in cache keys and DB rows.
const GrokReaderName = "grok"

const grokScannerMaxTokenSize = 64 * 1024 * 1024

// grokReader reads Grok CLI sessions from ~/.grok/sessions/ (or $GROK_HOME/sessions).
// Each session lives under <encoded-cwd>/<session-id>/ with summary.json metadata
// and chat_history.jsonl conversation turns.
type grokReader struct{}

// NewGrokReader returns a Reader for Grok CLI transcripts.
func NewGrokReader() Reader { return &grokReader{} }

func (r *grokReader) Name() string { return GrokReaderName }

func (r *grokReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	root, err := grokSessionsRoot(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	cwdGroups, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read grok sessions: %w", err)
	}

	matcher := newRepoMatcher(ctx, opts.OriginCWD)
	var out []*Session

	for _, group := range cwdGroups {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !group.IsDir() {
			continue
		}
		groupPath := filepath.Join(root, group.Name())
		groupCWD := grokDecodeGroupCWD(groupPath, group.Name())
		entries, err := os.ReadDir(groupPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if !entry.IsDir() {
				continue
			}
			sessionDir := filepath.Join(groupPath, entry.Name())
			summaryPath := filepath.Join(sessionDir, "summary.json")
			info, err := os.Stat(summaryPath)
			if err != nil || info.IsDir() {
				continue
			}
			modTime := info.ModTime()
			if !opts.WindowStart.IsZero() && modTime.Before(opts.WindowStart) {
				continue
			}
			if !opts.WindowEnd.IsZero() && modTime.After(opts.WindowEnd.Add(time.Hour)) {
				continue
			}
			meta, err := grokPeekSummary(summaryPath)
			if err != nil || meta == nil {
				continue
			}
			if meta.subagent {
				continue
			}
			cwd := meta.cwd
			if cwd == "" {
				cwd = groupCWD
			}
			if cwd == "" || !matcher.matches(ctx, cwd) {
				continue
			}
			// Prefer chat_history for Load; skip sessions without it.
			historyPath := filepath.Join(sessionDir, "chat_history.jsonl")
			if _, err := os.Stat(historyPath); err != nil {
				continue
			}
			sessionID := meta.id
			if sessionID == "" {
				sessionID = entry.Name()
			}
			startedAt := meta.createdAt
			if startedAt.IsZero() {
				startedAt = modTime
			}
			lastActivity := meta.updatedAt
			if lastActivity.IsZero() {
				lastActivity = modTime
			}
			session := &Session{
				AgentName:     GrokReaderName,
				SessionID:     sessionID,
				CWD:           cwd,
				StartedAt:     startedAt,
				LastActivity:  lastActivity,
				LastMsgKey:    historyPath + "|" + lastActivity.UTC().Format(time.RFC3339Nano),
				startedAtPath: historyPath,
			}
			out = append(out, session)
		}
	}
	return out, nil
}

func (r *grokReader) Load(_ context.Context, s *Session) error {
	if s.startedAtPath == "" {
		return fmt.Errorf("grok: session has no path")
	}
	f, err := os.Open(s.startedAtPath)
	if err != nil {
		return fmt.Errorf("grok open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), grokScannerMaxTokenSize)
	var lastKey string
	var n int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg, ok := parseGrokChatRecord(line)
		if !ok || msg == nil {
			continue
		}
		n++
		lastKey = fmt.Sprintf("%s|%d", s.startedAtPath, n)
		s.Messages = append(s.Messages, *msg)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("grok scan: %w", err)
	}
	if lastKey != "" {
		s.LastMsgKey = lastKey
	}

	// Best-effort file paths from tool calls in updates.jsonl (same session dir).
	updatesPath := filepath.Join(filepath.Dir(s.startedAtPath), "updates.jsonl")
	if paths := grokToolPathsFromUpdates(updatesPath); len(paths) > 0 && len(s.Messages) > 0 {
		// Attach paths to the last assistant message so matching can use them.
		for i := len(s.Messages) - 1; i >= 0; i-- {
			if s.Messages[i].Role == RoleAssistant {
				s.Messages[i].FilePaths = appendUniqueStrings(s.Messages[i].FilePaths, paths...)
				break
			}
		}
	}
	return nil
}

// grokSessionsRoot returns the sessions directory. When homeOverride is set
// (tests), sessions live under <home>/.grok/sessions. Otherwise GROK_HOME is
// honored when set, falling back to ~/.grok/sessions.
func grokSessionsRoot(homeOverride string) (string, error) {
	if homeOverride != "" {
		return filepath.Join(homeOverride, ".grok", "sessions"), nil
	}
	if g := strings.TrimSpace(os.Getenv("GROK_HOME")); g != "" {
		return filepath.Join(g, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".grok", "sessions"), nil
}

// grokDecodeGroupCWD recovers the working directory from a session group name.
// Grok URL-encodes the cwd; when the encoded name is too long it uses a slug
// and records the original path in a .cwd file inside the group.
func grokDecodeGroupCWD(groupPath, name string) string {
	if raw, err := os.ReadFile(filepath.Join(groupPath, ".cwd")); err == nil {
		if cwd := strings.TrimSpace(string(raw)); cwd != "" {
			return cwd
		}
	}
	if decoded, err := url.PathUnescape(name); err == nil && decoded != "" && decoded != name {
		return decoded
	}
	if decoded, err := url.QueryUnescape(name); err == nil && decoded != "" {
		return decoded
	}
	return ""
}

type grokSummaryMeta struct {
	id        string
	cwd       string
	createdAt time.Time
	updatedAt time.Time
	subagent  bool
}

func grokPeekSummary(path string) (*grokSummaryMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var summary struct {
		Info struct {
			ID  string `json:"id"`
			CWD string `json:"cwd"`
		} `json:"info"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
		LastActive  string `json:"last_active_at"`
		SessionKind string `json:"session_kind"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, err
	}
	meta := &grokSummaryMeta{
		id:       summary.Info.ID,
		cwd:      summary.Info.CWD,
		subagent: strings.EqualFold(summary.SessionKind, "subagent"),
	}
	meta.createdAt = grokParseTime(summary.CreatedAt)
	meta.updatedAt = grokParseTime(summary.UpdatedAt)
	if last := grokParseTime(summary.LastActive); !last.IsZero() {
		meta.updatedAt = last
	}
	return meta, nil
}

func grokParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func parseGrokChatRecord(line []byte) (*Message, bool) {
	var raw struct {
		Type            string          `json:"type"`
		Content         json.RawMessage `json:"content"`
		SyntheticReason string          `json:"synthetic_reason"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, false
	}

	switch raw.Type {
	case "user":
		if raw.SyntheticReason != "" {
			return nil, true
		}
		text := strings.TrimSpace(grokContentText(raw.Content))
		if text == "" || isGrokSyntheticUserText(text) {
			return nil, true
		}
		text = unwrapGrokUserQuery(text)
		if text == "" {
			return nil, true
		}
		return &Message{
			Role:      RoleUser,
			Text:      text,
			FilePaths: scanFilePathsInText(text),
		}, true
	case "assistant":
		text := strings.TrimSpace(grokContentText(raw.Content))
		if text == "" {
			return nil, true
		}
		return &Message{
			Role:      RoleAssistant,
			Text:      text,
			FilePaths: scanFilePathsInText(text),
		}, true
	default:
		return nil, true
	}
}

// grokContentText extracts plain text from chat_history content, which may be
// a string or a list of {type,text} blocks.
func grokContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type != "" && block.Type != "text" {
			continue
		}
		b.WriteString(block.Text)
	}
	return b.String()
}

// isGrokSyntheticUserText filters injected context that is not user intent.
func isGrokSyntheticUserText(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "<system-reminder>") {
		return true
	}
	if strings.HasPrefix(t, "<user_info>") && !strings.Contains(t, "<user_query>") {
		return true
	}
	if strings.HasPrefix(t, "You are Grok") || strings.HasPrefix(t, "You are a Grok") {
		return true
	}
	return false
}

// unwrapGrokUserQuery extracts the body of a <user_query>...</user_query>
// wrapper when present; otherwise returns the original text.
func unwrapGrokUserQuery(text string) string {
	const open = "<user_query>"
	const close = "</user_query>"
	start := strings.Index(text, open)
	if start < 0 {
		return text
	}
	start += len(open)
	end := strings.Index(text[start:], close)
	if end < 0 {
		return strings.TrimSpace(text[start:])
	}
	return strings.TrimSpace(text[start : start+end])
}

// grokToolPathsFromUpdates scans updates.jsonl for tool_call rawInput paths.
func grokToolPathsFromUpdates(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), grokScannerMaxTokenSize)
	var paths []string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Params struct {
				Update struct {
					SessionUpdate string         `json:"sessionUpdate"`
					RawInput      map[string]any `json:"rawInput"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Params.Update.SessionUpdate != "tool_call" || ev.Params.Update.RawInput == nil {
			continue
		}
		// Grok tools use snake_case path keys (file_path, target_directory, ...).
		paths = append(paths, extractToolPaths(ev.Params.Update.RawInput)...)
		for _, key := range []string{"target_directory", "target_file", "path", "file"} {
			if s, ok := ev.Params.Update.RawInput[key].(string); ok && s != "" {
				paths = append(paths, s)
			}
		}
	}
	return uniqueStrings(paths)
}

func appendUniqueStrings(base []string, extra ...string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, s := range base {
		seen[s] = struct{}{}
	}
	for _, s := range extra {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		base = append(base, s)
	}
	return base
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

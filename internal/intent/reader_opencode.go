package intent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// OpenCodeReaderName is the agent name used in cache keys and DB rows.
const OpenCodeReaderName = "opencode"

// opencodeReader reads OpenCode session/message/part rows from
// $XDG_DATA_HOME/opencode/opencode.db, falling back to
// ~/.local/share/opencode/opencode.db.
type opencodeReader struct{}

// NewOpenCodeReader returns a Reader for OpenCode transcripts.
func NewOpenCodeReader() Reader { return &opencodeReader{} }

func (r *opencodeReader) Name() string { return OpenCodeReaderName }

func (r *opencodeReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	dbPath, err := resolveOpenCodeDB(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, fmt.Errorf("opencode open: %w", err)
	}
	defer db.Close()

	matcher := newRepoMatcher(ctx, opts.OriginCWD)
	// OpenCode timestamps are unix milliseconds.
	winStart := opts.WindowStart.UnixMilli()
	winEnd := opts.WindowEnd.Add(time.Hour).UnixMilli()

	rows, err := db.QueryContext(ctx,
		`SELECT id, directory, time_created, time_updated FROM session
		 WHERE time_updated >= ? AND time_created <= ?
		 ORDER BY time_updated DESC LIMIT 200`,
		winStart, winEnd)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		var (
			id, directory            string
			timeCreated, timeUpdated int64
		)
		if err := rows.Scan(&id, &directory, &timeCreated, &timeUpdated); err != nil {
			continue
		}
		if !matcher.matches(ctx, directory) {
			continue
		}
		out = append(out, &Session{
			AgentName:     OpenCodeReaderName,
			SessionID:     id,
			CWD:           directory,
			StartedAt:     time.UnixMilli(timeCreated).UTC(),
			LastActivity:  time.UnixMilli(timeUpdated).UTC(),
			LastMsgKey:    fmt.Sprintf("%d", timeUpdated),
			startedAtPath: dbPath,
		})
	}
	return out, rows.Err()
}

func (r *opencodeReader) Load(ctx context.Context, s *Session) error {
	if s.startedAtPath == "" {
		return fmt.Errorf("opencode: missing db path")
	}
	db, err := sql.Open("sqlite", s.startedAtPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return fmt.Errorf("opencode open: %w", err)
	}
	defer db.Close()

	// Map message id → role using the role field embedded in message.data.
	msgRows, err := db.QueryContext(ctx,
		`SELECT id, time_created, data FROM message WHERE session_id = ? ORDER BY time_created, id`, s.SessionID)
	if err != nil {
		return fmt.Errorf("opencode messages: %w", err)
	}
	type msgInfo struct {
		role      Role
		timestamp time.Time
	}
	msgs := map[string]msgInfo{}
	var ordered []string
	for msgRows.Next() {
		var id, data string
		var tc int64
		if err := msgRows.Scan(&id, &tc, &data); err != nil {
			continue
		}
		var meta struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal([]byte(data), &meta)
		role := RoleAssistant
		if strings.EqualFold(meta.Role, "user") {
			role = RoleUser
		}
		msgs[id] = msgInfo{role: role, timestamp: time.UnixMilli(tc).UTC()}
		ordered = append(ordered, id)
	}
	msgRows.Close()

	// Walk parts in chronological order; bucket by message id.
	partRows, err := db.QueryContext(ctx,
		`SELECT message_id, data FROM part WHERE session_id = ? ORDER BY time_created, id`, s.SessionID)
	if err != nil {
		return fmt.Errorf("opencode parts: %w", err)
	}
	defer partRows.Close()

	type aggregated struct {
		text  strings.Builder
		paths []string
	}
	agg := map[string]*aggregated{}
	for partRows.Next() {
		var msgID, data string
		if err := partRows.Scan(&msgID, &data); err != nil {
			continue
		}
		var part struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Tool  string          `json:"tool"`
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal([]byte(data), &part); err != nil {
			continue
		}
		bucket := agg[msgID]
		if bucket == nil {
			bucket = &aggregated{}
			agg[msgID] = bucket
		}
		switch part.Type {
		case "text":
			if part.Text != "" {
				bucket.text.WriteString(part.Text)
				bucket.text.WriteString("\n")
			}
		case "tool":
			if len(part.State) > 0 {
				var st struct {
					Input map[string]any `json:"input"`
				}
				if err := json.Unmarshal(part.State, &st); err == nil && st.Input != nil {
					bucket.paths = append(bucket.paths, extractToolPaths(st.Input)...)
				}
			}
		}
	}

	// Reassemble preserving message order.
	for _, id := range ordered {
		bucket := agg[id]
		if bucket == nil {
			continue
		}
		text := strings.TrimSpace(bucket.text.String())
		if text == "" && len(bucket.paths) == 0 {
			continue
		}
		info := msgs[id]
		s.Messages = append(s.Messages, Message{
			Role:      info.role,
			Text:      text,
			FilePaths: bucket.paths,
			Timestamp: info.timestamp,
		})
	}
	if len(ordered) > 0 {
		s.LastMsgKey = ordered[len(ordered)-1]
	}
	return nil
}

// resolveOpenCodeDB picks the path to opencode.db, honoring XDG_DATA_HOME
// and falling back to ~/.local/share/opencode/opencode.db.
func resolveOpenCodeDB(homeOverride string) (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.db"), nil
	}
	home, err := resolveHome(homeOverride)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db"), nil
}

package intent

import (
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

// RovoDevReaderName is the agent name used in cache keys and DB rows.
const RovoDevReaderName = "rovodev"

// rovodevReader reads Atlassian Rovo Dev session JSON files from
// ~/.rovodev/sessions/<session-id>/.
type rovodevReader struct{}

// NewRovoDevReader returns a Reader for Rovo Dev transcripts.
func NewRovoDevReader() Reader { return &rovodevReader{} }

func (r *rovodevReader) Name() string { return RovoDevReaderName }

func (r *rovodevReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	home, err := resolveHome(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".rovodev", "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("rovodev sessions: %w", err)
	}

	matcher := newRepoMatcher(ctx, opts.OriginCWD)
	var out []*Session

	for _, dir := range entries {
		if !dir.IsDir() {
			continue
		}
		sessionDir := filepath.Join(root, dir.Name())
		ctxPath := filepath.Join(sessionDir, "session_context.json")
		info, err := os.Stat(ctxPath)
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
		meta, err := rovodevPeek(sessionDir)
		if err != nil || meta == nil {
			continue
		}
		if !matcher.matches(ctx, meta.workspace) {
			continue
		}
		out = append(out, &Session{
			AgentName:     RovoDevReaderName,
			SessionID:     dir.Name(),
			CWD:           meta.workspace,
			StartedAt:     meta.startedAt,
			LastActivity:  modTime,
			LastMsgKey:    modTime.UTC().Format(time.RFC3339Nano),
			startedAtPath: sessionDir,
		})
	}
	return out, nil
}

func (r *rovodevReader) Load(_ context.Context, s *Session) error {
	if s.startedAtPath == "" {
		return fmt.Errorf("rovodev: missing session path")
	}
	ctxPath := filepath.Join(s.startedAtPath, "session_context.json")
	data, err := os.ReadFile(ctxPath)
	if err != nil {
		return fmt.Errorf("rovodev read: %w", err)
	}
	var doc struct {
		Conversation []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"conversation"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("rovodev parse: %w", err)
	}
	for _, m := range doc.Conversation {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		role := RoleAssistant
		if strings.EqualFold(m.Role, "user") {
			role = RoleUser
		}
		s.Messages = append(s.Messages, Message{
			Role:      role,
			Text:      text,
			FilePaths: scanFilePathsInText(text),
		})
	}
	return nil
}

type rovodevMetadata struct {
	workspace string
	startedAt time.Time
}

// rovodevPeek reads the small metadata.json next to session_context.json
// to learn the workspace path and start time without parsing the full
// conversation.
func rovodevPeek(sessionDir string) (*rovodevMetadata, error) {
	// Try metadata.json first.
	if data, err := os.ReadFile(filepath.Join(sessionDir, "metadata.json")); err == nil {
		var raw struct {
			Workspace string `json:"workspace"`
			Title     string `json:"title"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(data, &raw); err == nil && raw.Workspace != "" {
			started, _ := time.Parse(time.RFC3339, raw.CreatedAt)
			return &rovodevMetadata{workspace: raw.Workspace, startedAt: started}, nil
		}
	}
	// Fall back to peeking at session_context.json's top-level workspace field.
	if data, err := os.ReadFile(filepath.Join(sessionDir, "session_context.json")); err == nil {
		var raw struct {
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal(data, &raw); err == nil && raw.Workspace != "" {
			return &rovodevMetadata{workspace: raw.Workspace}, nil
		}
	}
	return nil, nil
}

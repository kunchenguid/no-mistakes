package intent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func buildRovoDevSession(t *testing.T, repoCWD string) string {
	t.Helper()
	home := t.TempDir()
	sessionDir := filepath.Join(home, ".rovodev", "sessions", "abc-123")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(map[string]string{
		"workspace":  repoCWD,
		"title":      "add foo",
		"created_at": "2026-04-18T02:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	convo, err := json.Marshal(map[string]any{
		"workspace": repoCWD,
		"conversation": []map[string]string{
			{"role": "user", "content": "please add a foo function in internal/foo.go"},
			{"role": "assistant", "content": "done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "session_context.json"), convo, 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestRovoDevReader_DiscoverAndLoad(t *testing.T) {
	repoCWD := t.TempDir()
	home := buildRovoDevSession(t, repoCWD)

	r := NewRovoDevReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Now().Add(-24 * time.Hour),
		WindowEnd:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.SessionID != "abc-123" {
		t.Errorf("SessionID = %q", s.SessionID)
	}

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("messages = %+v", s.Messages)
	}
	if s.Messages[0].Role != RoleUser {
		t.Errorf("first role = %v", s.Messages[0].Role)
	}
}

func TestRovoDevReader_FiltersByWorkspace(t *testing.T) {
	repoA := t.TempDir()
	home := buildRovoDevSession(t, repoA)

	r := NewRovoDevReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   t.TempDir(), // unrelated path
		WindowStart: time.Now().Add(-24 * time.Hour),
		WindowEnd:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestRovoDevReader_NoSessionsDir(t *testing.T) {
	r := NewRovoDevReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

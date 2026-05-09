package intent

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// buildCodexFixture writes a state_5.sqlite + a rollout JSONL referenced by
// it, with the supplied cwd. The rollout has user, assistant, and tool-call
// turns so we can verify they're all parsed correctly.
func buildCodexFixture(t *testing.T, cwd string) (homeDir, rolloutPath string) {
	t.Helper()
	homeDir = t.TempDir()
	codexDir := filepath.Join(homeDir, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rolloutPath = filepath.Join(codexDir, "sessions", "2026", "04", "rollout-thread-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := strings.Join([]string{
		// User turn via event_msg envelope.
		`{"type":"event_msg","payload":{"type":"user_message","message":"please add a Bar() helper to internal/foo.go"}}`,
		// Assistant text reply.
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"on it - editing internal/foo.go now"}]}}`,
		// Tool call: shell command. File paths should be captured for matching, but text empty.
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"sed\",\"-i\",\"\",\"s/old/new/\",\"internal/foo.go\"]}"}}`,
		// Another user turn via response_item shape.
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"thanks, also update internal/bar.go"}]}}`,
		// Non-content envelope - should be skipped.
		`{"type":"turn_context","payload":{}}`,
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(codexDir, "state_5.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		cwd TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		rollout_path TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(
		`INSERT INTO threads (id, cwd, created_at, updated_at, rollout_path) VALUES (?, ?, ?, ?, ?)`,
		"thread-1", cwd, now, now, rolloutPath,
	); err != nil {
		t.Fatal(err)
	}
	return homeDir, rolloutPath
}

func TestCodexReader_ParsesAllTurnsFromRollout(t *testing.T) {
	repoCWD := t.TempDir()
	home, _ := buildCodexFixture(t, repoCWD)

	r := NewCodexReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if len(s.Messages) != 0 {
		t.Errorf("Discover should not populate Messages, got %d", len(s.Messages))
	}

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Expect: user(event_msg) + assistant(text) + assistant(tool_call paths only) + user(response_item)
	if len(s.Messages) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(s.Messages), s.Messages)
	}

	if s.Messages[0].Role != RoleUser || !strings.Contains(s.Messages[0].Text, "Bar()") {
		t.Errorf("turn 0 wrong: %+v", s.Messages[0])
	}
	if s.Messages[1].Role != RoleAssistant || !strings.Contains(s.Messages[1].Text, "editing") {
		t.Errorf("turn 1 wrong: %+v", s.Messages[1])
	}

	// Tool-call message must NOT contain the shell command in Text.
	if s.Messages[2].Text != "" {
		t.Errorf("tool call turn leaked text: %q", s.Messages[2].Text)
	}
	foundPath := false
	for _, p := range s.Messages[2].FilePaths {
		if strings.Contains(p, "internal/foo.go") || p == "foo.go" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("tool call turn should have captured internal/foo.go, got %v", s.Messages[2].FilePaths)
	}

	// Second user turn (via response_item shape).
	if s.Messages[3].Role != RoleUser || !strings.Contains(s.Messages[3].Text, "bar.go") {
		t.Errorf("turn 3 wrong: %+v", s.Messages[3])
	}
}

func TestCodexReader_FiltersByCWD(t *testing.T) {
	home, _ := buildCodexFixture(t, "/some/other/path")
	r := NewCodexReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   "/different",
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestCodexReader_NoStateDB(t *testing.T) {
	r := NewCodexReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions when no DB, got %d", len(sessions))
	}
}

func TestCodexReader_MissingRollout(t *testing.T) {
	repoCWD := t.TempDir()
	home, rolloutPath := buildCodexFixture(t, repoCWD)
	if err := os.Remove(rolloutPath); err != nil {
		t.Fatal(err)
	}
	r := NewCodexReader()
	sessions, _ := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(time.Hour),
	})
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	// Load must error gracefully when the rollout is gone, not panic.
	if err := r.Load(context.Background(), sessions[0]); err == nil {
		t.Error("expected error when rollout missing")
	}
}

func TestResolveCodexStateDB_PicksHighestVersion(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"state_4.sqlite", "state_5.sqlite", "state_6.sqlite", "unrelated.sqlite"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveCodexStateDB(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "state_6.sqlite" {
		t.Errorf("picked %s, want state_6.sqlite", got)
	}
}

// Lexicographic sort would rank state_9 ahead of state_10 (because '9' > '1');
// numeric sort must pick state_10. Once Codex bumps past state_9 this is the
// difference between reading the live DB and reading a stale one.
func TestResolveCodexStateDB_NumericSortPastNine(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"state_9.sqlite", "state_10.sqlite", "state_11.sqlite", "state_5.sqlite"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveCodexStateDB(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "state_11.sqlite" {
		t.Errorf("picked %s, want state_11.sqlite (numeric sort)", got)
	}
}

// Non-numeric suffixes (e.g. backups) must not override a real numbered DB.
func TestResolveCodexStateDB_IgnoresNonNumericSuffix(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"state_5.sqlite", "state_backup.sqlite"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveCodexStateDB(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "state_5.sqlite" {
		t.Errorf("picked %s, want state_5.sqlite (numeric must outrank non-numeric)", got)
	}
}

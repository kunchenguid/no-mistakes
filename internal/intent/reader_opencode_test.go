package intent

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func buildOpenCodeDB(t *testing.T, sessionDir string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "opencode.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY,
			directory TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL
		)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
	}
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO session VALUES (?, ?, ?, ?)`,
		"sess-1", sessionDir, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES (?, ?, ?, ?)`,
		"msg-1", "sess-1", now, `{"role":"user"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES (?, ?, ?, ?)`,
		"msg-2", "sess-1", now+1, `{"role":"assistant"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?)`,
		"p1", "msg-1", "sess-1", now, `{"type":"text","text":"please add a bar to foo.go"}`); err != nil {
		t.Fatal(err)
	}
	// Reasoning part should not show up in any message text.
	if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?)`,
		"p2", "msg-2", "sess-1", now+1, `{"type":"reasoning","text":"thinking..."}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?)`,
		"p3", "msg-2", "sess-1", now+2, `{"type":"text","text":"done"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?)`,
		"p4", "msg-2", "sess-1", now+3, `{"type":"tool","tool":"edit","state":{"input":{"file_path":"foo.go"}}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?)`,
		"p5", "msg-2", "sess-1", now+4, `{"type":"tool","tool":"edit","state":{"input":{"filePath":"internal/tui/pipeline.go"}}}`); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestOpenCodeReader_DiscoverAndLoad(t *testing.T) {
	repoCWD := t.TempDir()
	home := buildOpenCodeDB(t, repoCWD)
	t.Setenv("XDG_DATA_HOME", "")

	r := NewOpenCodeReader()
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

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("messages = %+v", s.Messages)
	}
	if s.Messages[0].Role != RoleUser {
		t.Errorf("first message role = %v", s.Messages[0].Role)
	}
	if s.Messages[1].Role != RoleAssistant {
		t.Errorf("second message role = %v", s.Messages[1].Role)
	}
	// Reasoning text must NOT appear in assistant text.
	if got := s.Messages[1].Text; got == "thinking..." || got == "" {
		t.Errorf("assistant text wrong, got %q", got)
	}
	foundPath := false
	for _, p := range s.Messages[1].FilePaths {
		if p == "foo.go" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("expected foo.go in tool input paths, got %v", s.Messages[1].FilePaths)
	}
	foundCamelPath := false
	for _, p := range s.Messages[1].FilePaths {
		if p == "internal/tui/pipeline.go" {
			foundCamelPath = true
		}
	}
	if !foundCamelPath {
		t.Errorf("expected camelCase filePath in tool input paths, got %v", s.Messages[1].FilePaths)
	}
}

func TestOpenCodeReader_DiscoverAcceptsSameRemoteDifferentCheckout(t *testing.T) {
	originCWD := initGitRepoWithRemote(t, filepath.Join(t.TempDir(), "origin"), "git@github.com:kunchenguid/no-mistakes.git")
	sessionCWD := initGitRepoWithRemote(t, filepath.Join(t.TempDir(), "treehouse"), "https://github.com/kunchenguid/no-mistakes.git")
	home := buildOpenCodeDB(t, sessionCWD)
	t.Setenv("XDG_DATA_HOME", "")

	r := NewOpenCodeReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   originCWD,
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].CWD != sessionCWD {
		t.Fatalf("session cwd = %q, want %q", sessions[0].CWD, sessionCWD)
	}
}

func TestOpenCodeReader_NoDB(t *testing.T) {
	r := NewOpenCodeReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func initGitRepoWithRemote(t *testing.T, dir, remote string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", remote},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

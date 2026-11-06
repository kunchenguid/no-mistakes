package intent

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// Every transcript and SQLite row is invented, including the competing account.
func writeAccountTranscript(t *testing.T, backend, root, repo, text string) {
	t.Helper()
	if backend == "claude" {
		dir := filepath.Join(root, "projects", claudeProjectDirName(repo))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"user","cwd":` + jsonString(t, repo) + `,"timestamp":"2026-04-18T02:15:37Z","uuid":"same-message","message":{"role":"user","content":` + jsonString(t, text) + `}}`
		if err := os.WriteFile(filepath.Join(dir, "same-session.jsonl"), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := `{"type":"event_msg","payload":{"type":"user_message","message":` + jsonString(t, text) + `}}`
	if err := os.WriteFile(filepath.Join(root, "sessions", "same-session.jsonl"), []byte(rollout+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, cwd TEXT, created_at INTEGER, updated_at INTEGER, rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := d.Exec(`INSERT INTO threads VALUES (?, ?, ?, ?, ?)`, "same-session", repo, now, now, filepath.Join("sessions", "same-session.jsonl")); err != nil {
		t.Fatal(err)
	}
}

type accountSummarizer struct{}

func (accountSummarizer) Summarize(_ context.Context, s *Session, _ SummaryContext) (string, error) {
	return s.Messages[0].Text, nil
}

func TestExtractAccountHomes(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			home, repo := t.TempDir(), t.TempDir()
			defaultRoot := filepath.Join(home, "."+backend)
			selectedRoot := filepath.Join(t.TempDir(), "selected account with spaces")
			daemonRoot := filepath.Join(t.TempDir(), "daemon account")
			writeAccountTranscript(t, backend, defaultRoot, repo, "default account: edit internal/foo.go")
			writeAccountTranscript(t, backend, selectedRoot, repo, "selected account: edit internal/foo.go")
			writeAccountTranscript(t, backend, daemonRoot, repo, "daemon account: edit internal/foo.go")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CLAUDE_CONFIG_DIR", daemonRoot)
			t.Setenv("CODEX_HOME", daemonRoot)
			reader := NewClaudeReader()
			if backend == "codex" {
				reader = NewCodexReader()
			}
			cache := NewMemCache()
			extract := func(root string) (*Result, error) {
				p := ExtractParams{
					HomeDir: home, OriginCWD: repo, DiffFiles: []string{"internal/foo.go"},
					BaseTime: time.Now().Add(-time.Hour), HeadTime: time.Now().Add(time.Hour),
					Threshold: 0.2, Readers: []Reader{reader}, Summarizer: accountSummarizer{}, Cache: cache,
				}
				if root != "" {
					p.TranscriptHomes = &runenv.TranscriptHomes{ClaudeConfigDir: root, CodexHome: root}
				}
				return Extract(context.Background(), p)
			}
			for _, tc := range []struct{ root, want string }{
				{"", "default account"}, {selectedRoot, "selected account"}, {daemonRoot, "daemon account"}, {selectedRoot, "selected account"},
			} {
				got, err := extract(tc.root)
				if err != nil || got == nil || !strings.HasPrefix(got.Summary, tc.want+":") {
					t.Errorf("root %q: got %+v, err %v, want %s", tc.root, got, err, tc.want)
				}
			}
			for _, root := range []string{"", ".codex-work", "~/account", "invalid\x00account"} {
				homes := &runenv.TranscriptHomes{ClaudeConfigDir: root, CodexHome: root}
				sessions, err := reader.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repo, TranscriptHomes: homes})
				if len(sessions) != 0 || err == nil || !strings.Contains(err.Error(), "transcript home must be an absolute path") {
					t.Errorf("invalid home %q: sessions %+v, error %v, want no sessions and diagnostic", root, sessions, err)
				}
				if root != "" {
					if got, err := extract(root); got != nil || err != ErrNoMatch {
						t.Errorf("invalid home %q: got %+v, %v, want no match", root, got, err)
					}
				}
			}
			if got, err := extract(filepath.Join(t.TempDir(), "missing")); err != ErrNoMatch || got != nil {
				t.Errorf("missing selected home: got %+v, %v, want no match", got, err)
			}
			if runtime.GOOS != "windows" {
				locked := filepath.Join(t.TempDir(), "unreadable")
				writeAccountTranscript(t, backend, locked, repo, "unreadable account: edit internal/foo.go")
				if err := os.Chmod(locked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
				if _, err := os.ReadDir(locked); err == nil {
					t.Fatal("unreadable-home proof requires an unprivileged test user")
				}
				if got, err := extract(locked); err != ErrNoMatch || got != nil {
					t.Errorf("unreadable selected home: got %+v, %v, want no match", got, err)
				}
			}
		})
	}
}

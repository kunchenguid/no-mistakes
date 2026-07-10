package intent

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGrokReader_DiscoverAndLoad(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/repo"
	sessionID := "019ed40a-0863-72e0-923d-b0a22c9e1dd7"
	group := url.PathEscape(cwd)
	sessionDir := filepath.Join(home, ".grok", "sessions", group, sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	created := time.Date(2026, 6, 18, 7, 0, 0, 0, time.UTC)
	updated := created.Add(2 * time.Minute)
	writeGrokSummary(t, sessionDir, sessionID, cwd, created, updated, "")
	history := strings.Join([]string{
		`{"type":"system","content":"You are Grok"}`,
		`{"type":"user","content":[{"type":"text","text":"<user_info>\nOS: linux\n</user_info>"}]}`,
		`{"type":"user","content":[{"type":"text","text":"<system-reminder>\nskills\n</system-reminder>"}],"synthetic_reason":"skills"}`,
		`{"type":"user","content":[{"type":"text","text":"<user_query>\nAdd a --json flag to status\n</user_query>"}]}`,
		`{"type":"assistant","content":"I'll add the flag in internal/cli/status.go"}`,
		`{"type":"reasoning","content":null}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sessionDir, "chat_history.jsonl"), []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}
	updates := `{"params":{"update":{"sessionUpdate":"tool_call","rawInput":{"target_file":"internal/cli/status.go"}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "updates.jsonl"), []byte(updates), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewGrokReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:   home,
		OriginCWD: cwd,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover returned %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.AgentName != GrokReaderName {
		t.Errorf("AgentName = %q", s.AgentName)
	}
	if s.SessionID != sessionID {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if s.CWD != cwd {
		t.Errorf("CWD = %q, want %q", s.CWD, cwd)
	}

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (user+assistant); got %#v", len(s.Messages), s.Messages)
	}
	if s.Messages[0].Role != RoleUser || s.Messages[0].Text != "Add a --json flag to status" {
		t.Errorf("user message = %+v", s.Messages[0])
	}
	if s.Messages[1].Role != RoleAssistant {
		t.Errorf("assistant role = %q", s.Messages[1].Role)
	}
	if !containsString(s.Messages[1].FilePaths, "internal/cli/status.go") {
		t.Errorf("assistant FilePaths = %v, want status.go from tool call or text", s.Messages[1].FilePaths)
	}
}

func TestGrokReader_SkipsSubagentsAndMissingHistory(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/repo"
	group := url.PathEscape(cwd)
	root := filepath.Join(home, ".grok", "sessions", group)

	subDir := filepath.Join(root, "sub-1")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeGrokSummary(t, subDir, "sub-1", cwd, now, now, "subagent")
	if err := os.WriteFile(filepath.Join(subDir, "chat_history.jsonl"), []byte(`{"type":"user","content":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	emptyDir := filepath.Join(root, "no-hist")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGrokSummary(t, emptyDir, "no-hist", cwd, now, now, "")

	r := NewGrokReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: cwd})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestGrokReader_SkipsPipelineWorktreeSessions(t *testing.T) {
	// A native Grok run inside a no-mistakes worktree has the same origin
	// remote as the user's checkout, so normal repository matching accepts it.
	// Its prompt is pipeline machinery, not user intent, and must never be
	// considered for the next run.
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	remote := "https://github.com/kunchenguid/no-mistakes.git"
	originCWD := initGitRepoWithRemote(t, filepath.Join(t.TempDir(), "working"), remote)
	worktreeCWD := initGitRepoWithRemote(t, filepath.Join(nmHome, "worktrees", "repo", "run"), remote)

	home := t.TempDir()
	sessionID := "pipeline-run"
	// QueryEscape encodes the drive-letter colon on Windows, keeping the
	// encoded CWD valid as a single sessions-directory component.
	sessionDir := filepath.Join(home, ".grok", "sessions", url.QueryEscape(worktreeCWD), sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeGrokSummary(t, sessionDir, sessionID, worktreeCWD, now, now, "")
	if err := os.WriteFile(filepath.Join(sessionDir, "chat_history.jsonl"), []byte(`{"type":"user","content":"review the pipeline diff"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := NewGrokReader().Discover(context.Background(), DiscoverOpts{
		HomeDir:   home,
		OriginCWD: originCWD,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("pipeline worktree session must be excluded, got %d: %+v", len(sessions), sessions)
	}
}

func TestGrokPipelineWorktreeCWD_RecognizesDeletedWorktreeViaSymlink(t *testing.T) {
	realHome := t.TempDir()
	linkHome := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	t.Setenv("NM_HOME", realHome)
	if err := os.MkdirAll(filepath.Join(realHome, "worktrees", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	deletedWorktree := filepath.Join(linkHome, "worktrees", "repo", "completed-run")
	if !grokPipelineWorktreeCWD(deletedWorktree) {
		t.Fatal("deleted worktree through symlink must be recognized")
	}
}

func TestGrokReader_MissingRootIsEmpty(t *testing.T) {
	r := NewGrokReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected empty, got %d", len(sessions))
	}
}

func TestGrokDecodeGroupCWD_FromDotCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".cwd"), []byte("/real/path\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := grokDecodeGroupCWD(dir, "hashed-name"); got != "/real/path" {
		t.Errorf("got %q", got)
	}
}

func TestUnwrapGrokUserQuery(t *testing.T) {
	got := unwrapGrokUserQuery("<user_query>\nhello world\n</user_query>")
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
	if got := unwrapGrokUserQuery("plain"); got != "plain" {
		t.Errorf("plain = %q", got)
	}
}

func TestGrokContentText(t *testing.T) {
	if got := grokContentText(json.RawMessage(`"hi"`)); got != "hi" {
		t.Errorf("string content = %q", got)
	}
	if got := grokContentText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)); got != "ab" {
		t.Errorf("blocks = %q", got)
	}
}

func writeGrokSummary(t *testing.T, dir, id, cwd string, created, updated time.Time, kind string) {
	t.Helper()
	summary := map[string]any{
		"info": map[string]any{
			"id":  id,
			"cwd": cwd,
		},
		"created_at": created.Format(time.RFC3339Nano),
		"updated_at": updated.Format(time.RFC3339Nano),
	}
	if kind != "" {
		summary["session_kind"] = kind
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

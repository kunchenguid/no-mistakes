package intent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

type mutatingAgent struct {
	run func(context.Context, agent.RunOpts) (*agent.Result, error)
}

func (m mutatingAgent) Name() string { return "mutating" }
func (m mutatingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return m.run(ctx, opts)
}
func (m mutatingAgent) Close() error { return nil }

func TestAgentDisambiguatorRestoresAfterBranchSwitchWithDirtyConflict(t *testing.T) {
	ctx := context.Background()
	repo := initDisambiguatorTestRepo(t)
	mainHead := gitTestOutput(t, repo, "rev-parse", "HEAD")

	d := NewAgentDisambiguator(mutatingAgent{run: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		gitTestOutput(t, opts.CWD, "checkout", "other")
		if err := os.WriteFile(filepath.Join(opts.CWD, "conflict.txt"), []byte("mutated\n"), 0o644); err != nil {
			t.Fatalf("write mutation: %v", err)
		}
		return &agent.Result{Output: json.RawMessage(`{"session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
	}}, repo)

	_, err := d.Disambiguate(ctx, []string{"conflict.txt"}, []*Match{{Session: &Session{
		SessionID:    "s1",
		AgentName:    "test",
		LastActivity: time.Now(),
		Messages:     []Message{{Role: RoleUser, Text: "edit conflict.txt"}},
	}}})
	if err != nil {
		t.Fatalf("disambiguate: %v", err)
	}
	if got := gitTestOutput(t, repo, "branch", "--show-current"); got != "main" {
		t.Fatalf("branch = %q, want main", got)
	}
	if got := gitTestOutput(t, repo, "rev-parse", "HEAD"); got != mainHead {
		t.Fatalf("HEAD = %q, want %q", got, mainHead)
	}
	data, err := os.ReadFile(filepath.Join(repo, "conflict.txt"))
	if err != nil {
		t.Fatalf("read conflict.txt: %v", err)
	}
	if string(data) != "main\n" {
		t.Fatalf("conflict.txt = %q, want main", data)
	}
	if got := gitTestOutput(t, repo, "status", "--porcelain", "-uall"); got != "" {
		t.Fatalf("status = %q, want clean", got)
	}
}

func TestAgentDisambiguatorRemovesIgnoredSideEffects(t *testing.T) {
	ctx := context.Background()
	repo := initDisambiguatorTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	gitTestOutput(t, repo, "add", ".gitignore")
	gitTestOutput(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "ignore logs")

	d := NewAgentDisambiguator(mutatingAgent{run: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "ignored.log"), []byte("mutated\n"), 0o644); err != nil {
			t.Fatalf("write ignored mutation: %v", err)
		}
		return &agent.Result{Output: json.RawMessage(`{"session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
	}}, repo)

	_, err := d.Disambiguate(ctx, []string{"conflict.txt"}, []*Match{{Session: &Session{
		SessionID:    "s1",
		AgentName:    "test",
		LastActivity: time.Now(),
		Messages:     []Message{{Role: RoleUser, Text: "edit conflict.txt"}},
	}}})
	if err != nil {
		t.Fatalf("disambiguate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "ignored.log")); !os.IsNotExist(err) {
		t.Fatalf("ignored.log exists after cleanup, err = %v", err)
	}
	if got := gitTestOutput(t, repo, "status", "--porcelain", "-uall", "--ignored"); got != "" {
		t.Fatalf("status = %q, want clean", got)
	}
}

func initDisambiguatorTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitTestOutput(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "conflict.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	gitTestOutput(t, repo, "add", "conflict.txt")
	gitTestOutput(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "main")
	gitTestOutput(t, repo, "checkout", "-b", "other")
	if err := os.WriteFile(filepath.Join(repo, "conflict.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatalf("write other file: %v", err)
	}
	gitTestOutput(t, repo, "add", "conflict.txt")
	gitTestOutput(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "other")
	gitTestOutput(t, repo, "checkout", "main")
	return repo
}

func gitTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

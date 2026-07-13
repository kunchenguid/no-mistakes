package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		return &agent.Result{Output: json.RawMessage(`{"agent_name":"test","session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
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
		return &agent.Result{Output: json.RawMessage(`{"agent_name":"test","session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
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

func TestAgentDisambiguatorPreservesPreexistingIgnoredFiles(t *testing.T) {
	ctx := context.Background()
	repo := initDisambiguatorTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	gitTestOutput(t, repo, "add", ".gitignore")
	gitTestOutput(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "ignore logs")
	if err := os.WriteFile(filepath.Join(repo, "keep.log"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("write preexisting ignored file: %v", err)
	}

	d := NewAgentDisambiguator(mutatingAgent{run: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "new.log"), []byte("side effect\n"), 0o644); err != nil {
			t.Fatalf("write ignored mutation: %v", err)
		}
		if err := os.WriteFile(filepath.Join(opts.CWD, "conflict.txt"), []byte("mutated\n"), 0o644); err != nil {
			t.Fatalf("write tracked mutation: %v", err)
		}
		return &agent.Result{Output: json.RawMessage(`{"agent_name":"test","session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
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
	data, err := os.ReadFile(filepath.Join(repo, "keep.log"))
	if err != nil {
		t.Fatalf("read preexisting ignored file: %v", err)
	}
	if string(data) != "keep\n" {
		t.Fatalf("keep.log = %q, want keep", data)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.log")); !os.IsNotExist(err) {
		t.Fatalf("new.log exists after cleanup, err = %v", err)
	}
}

func TestAgentDisambiguatorPreservesPreexistingIgnoredDirectory(t *testing.T) {
	ctx := context.Background()
	repo := initDisambiguatorTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("cache/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	gitTestOutput(t, repo, "add", ".gitignore")
	gitTestOutput(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "ignore cache")
	if err := os.Mkdir(filepath.Join(repo, "cache"), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "cache", "state.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatalf("write ignored file: %v", err)
	}

	d := NewAgentDisambiguator(mutatingAgent{run: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "cache", "state.txt"), []byte("after\n"), 0o644); err != nil {
			t.Fatalf("mutate ignored file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(opts.CWD, "conflict.txt"), []byte("mutated\n"), 0o644); err != nil {
			t.Fatalf("write tracked mutation: %v", err)
		}
		return &agent.Result{Output: json.RawMessage(`{"agent_name":"test","session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
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
	data, err := os.ReadFile(filepath.Join(repo, "cache", "state.txt"))
	if err != nil {
		t.Fatalf("read ignored file: %v", err)
	}
	if string(data) != "after\n" {
		t.Fatalf("cache/state.txt = %q, want after", data)
	}
	tracked, err := os.ReadFile(filepath.Join(repo, "conflict.txt"))
	if err != nil {
		t.Fatalf("read conflict.txt: %v", err)
	}
	if string(tracked) != "main\n" {
		t.Fatalf("conflict.txt = %q, want main", tracked)
	}
}

func TestAgentDisambiguatorRemovesNestedGitRepositorySideEffect(t *testing.T) {
	ctx := context.Background()
	repo := initDisambiguatorTestRepo(t)

	d := NewAgentDisambiguator(mutatingAgent{run: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		nested := filepath.Join(opts.CWD, "nested")
		if err := os.Mkdir(nested, 0o755); err != nil {
			t.Fatalf("mkdir nested: %v", err)
		}
		gitTestOutput(t, nested, "init", "-b", "main")
		gitTestOutput(t, nested, "config", "core.autocrlf", "false")
		if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("nested\n"), 0o644); err != nil {
			t.Fatalf("write nested file: %v", err)
		}
		return &agent.Result{Output: json.RawMessage(`{"agent_name":"test","session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
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
	if _, err := os.Stat(filepath.Join(repo, "nested")); !os.IsNotExist(err) {
		t.Fatalf("nested repo exists after cleanup, err = %v", err)
	}
}

func TestAgentDisambiguatorPreservesPreexistingIgnoredSymlink(t *testing.T) {
	ctx := context.Background()
	repo := initDisambiguatorTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	gitTestOutput(t, repo, "add", ".gitignore")
	gitTestOutput(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "ignore logs")
	if err := os.Symlink("missing", filepath.Join(repo, "keep.log")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	d := NewAgentDisambiguator(mutatingAgent{run: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "conflict.txt"), []byte("mutated\n"), 0o644); err != nil {
			t.Fatalf("write tracked mutation: %v", err)
		}
		return &agent.Result{Output: json.RawMessage(`{"agent_name":"test","session_id":"s1","confidence":0.9,"reason":"matched"}`)}, nil
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
	target, err := os.Readlink(filepath.Join(repo, "keep.log"))
	if err != nil {
		t.Fatalf("read ignored symlink: %v", err)
	}
	if target != "missing" {
		t.Fatalf("keep.log target = %q, want missing", target)
	}
}

func TestAgentDisambiguatorReturnsCleanupErrorAfterAgentError(t *testing.T) {
	ctx := context.Background()
	repo := initDisambiguatorTestRepo(t)

	d := NewAgentDisambiguator(mutatingAgent{run: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "conflict.txt"), []byte("mutated\n"), 0o644); err != nil {
			t.Fatalf("write tracked mutation: %v", err)
		}
		if err := os.Rename(filepath.Join(opts.CWD, ".git"), filepath.Join(opts.CWD, ".git-disabled")); err != nil {
			t.Fatalf("disable git dir: %v", err)
		}
		return nil, fmt.Errorf("agent failed")
	}}, repo)

	_, err := d.Disambiguate(ctx, []string{"conflict.txt"}, []*Match{{Session: &Session{
		SessionID:    "s1",
		AgentName:    "test",
		LastActivity: time.Now(),
		Messages:     []Message{{Role: RoleUser, Text: "edit conflict.txt"}},
	}}})
	if !errors.Is(err, ErrDisambiguatorCleanup) {
		t.Fatalf("disambiguate error = %v, want ErrDisambiguatorCleanup", err)
	}
}

func initDisambiguatorTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitTestOutput(t, repo, "init", "-b", "main")
	gitTestOutput(t, repo, "config", "core.autocrlf", "false")
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

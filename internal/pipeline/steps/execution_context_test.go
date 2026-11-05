package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestExecutionContextPromptSection_Mentions(t *testing.T) {
	t.Parallel()
	got := executionContextPromptSection(t.TempDir())
	for _, want := range []string{
		"isolated git worktree",
		"pointer file",
		"do not search the filesystem",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("execution context section missing %q; got:\n%s", want, got)
		}
	}
}

func assertBackingGitContract(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{
		"selective read-only access to this worktree's authentic backing Git metadata",
		"original absolute paths",
		"original `.git` pointer",
		"Git/common directories, index/history, and inherited gate markers",
		"operational databases/configuration, control sockets, credentials, trust stores, unrelated repositories, or foreign endpoints",
		"report the blocker",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("generated execution context missing %q", want)
		}
	}
	if strings.Contains(prompt, "Do not modify or read from the gate's bare repository") {
		t.Error("generated execution context still bans authentic backing Git reads")
	}
}

func TestExecutionContextPromptSection_BackingGitContract(t *testing.T) {
	t.Parallel()
	assertBackingGitContract(t, executionContextPromptSection(t.TempDir()))
}

func TestExecutionContext_TestRepairAndEvidenceDelivery(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"no runtime surface","findings":[],"tested":["inspected generated prompt contract"],"testing_summary":"prompt contract only","artifacts":[],"scenarios":[{"name":"generated instruction contract","result":"untested","live":false,"evidence":"","reason":"synthetic prompt-delivery fixture has no runtime product surface"}],"verdict":"no-surface"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"test-1","severity":"error","description":"repair requested"}]}`
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("got %d calls, want repair and unconditional evidence", len(ag.calls))
	}
	for _, call := range ag.calls {
		assertBackingGitContract(t, call.Prompt)
	}
}

// This fixture checks metadata sufficiency and preservation during read-only
// Git commands. It does not implement or certify an OS sandbox.
func TestExecutionContext_LinkedGitReadsPreserveIdentity(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(root, "seed")
	gate := filepath.Join(root, "gate.git")
	work := filepath.Join(root, "assigned")
	gitCmd(t, root, "init", seed)
	source := []byte("authentic assigned source\n")
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "source.txt")
	gitCmd(t, seed, "commit", "-m", "fixture history")
	head := gitCmd(t, seed, "rev-parse", "HEAD")
	blob := gitCmd(t, seed, "rev-parse", "HEAD:source.txt")
	gitCmd(t, root, "clone", "--bare", seed, gate)
	gitCmd(t, root, "--git-dir", gate, "worktree", "add", "--detach", work, head)
	gitDir := filepath.Join(gate, "worktrees", "assigned")
	paths := []string{filepath.Join(work, "source.txt"), filepath.Join(work, ".git"), filepath.Join(gitDir, "index"), filepath.Join(gate, "config")}
	before := make(map[string][]byte)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	if !bytes.HasPrefix(before[filepath.Join(work, ".git")], []byte("gitdir: ")) {
		t.Fatal("fixture must retain the authentic linked-worktree pointer")
	}
	assertBackingGitContract(t, executionContextPromptSection(work))
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "--show-toplevel"}, filepath.ToSlash(work)},
		{[]string{"rev-parse", "--absolute-git-dir"}, filepath.ToSlash(gitDir)},
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, filepath.ToSlash(gate)},
		{[]string{"rev-parse", "HEAD"}, head},
		{[]string{"log", "-1", "--format=%s"}, "fixture history"},
		{[]string{"ls-files", "--stage"}, "100644 " + blob + " 0\tsource.txt"},
	} {
		if got := gitCmd(t, work, check.args...); got != check.want {
			t.Errorf("git %v = %q, want %q", check.args, got, check.want)
		}
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before[path], after) {
			t.Errorf("read-only Git inspection changed %s", path)
		}
	}
}

// The section is injected into review/test/lint/document/pr step prompts.
// It must be task-neutral - words like "review" or "lint" leak the wrong
// framing into other steps.
func TestExecutionContextPromptSection_TaskNeutral(t *testing.T) {
	t.Parallel()
	// The injected workDir is a t.TempDir path whose name embeds this test's
	// own name; neutrality applies to the static template, so scan without it.
	dir := t.TempDir()
	got := strings.Replace(executionContextPromptSection(dir), dir, "<WORKDIR>", -1)
	for _, banned := range []string{
		"reviewed",
		"review",
		"linted",
		"lint",
		"tested",
		"test",
		"document",
	} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("execution context section contains task-specific word %q; should be neutral. Section:\n%s", banned, got)
		}
	}
}

func TestExecutionContextPromptSection_NewlineSafe(t *testing.T) {
	t.Parallel()
	got := executionContextPromptSection(t.TempDir())
	if !strings.HasPrefix(got, "\n") {
		t.Error("expected leading newline so callers can append cleanly")
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("expected trailing newline")
	}
}

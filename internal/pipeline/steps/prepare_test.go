package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEnsurePrepared_RunsOnceAndKeepsOnlyIgnoredMaterialization(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("first preparation: %v", err)
	}
	// Recreate run-scoped memory to model daemon recovery in the same worktree;
	// the durable worktree marker must still prevent a second preparation.
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepLint); err != nil {
		t.Fatalf("second preparation: %v", err)
	}

	count, err := os.ReadFile(filepath.Join(dir, ".deps", "count"))
	if err != nil {
		t.Fatalf("read materialized dependency marker: %v", err)
	}
	if got := strings.Count(string(count), "prepared"); got != 1 {
		t.Fatalf("prepare executions = %d, want 1; marker=%q", got, count)
	}
	base, err := os.ReadFile(filepath.Join(dir, "base.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(base) != "base content" {
		t.Fatalf("tracked setup mutation survived: %q", base)
	}
	if _, err := os.Stat(filepath.Join(dir, "prepare.tmp")); !os.IsNotExist(err) {
		t.Fatalf("ordinary untracked setup artifact survived: %v", err)
	}
}

func TestConfiguredTestAndLintSharePreparation(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	// A configured baseline no longer replaces Test's evidence turn.
	// Keep that turn in the shared-preparation journey without a real model.
	evidenceCalls := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		evidenceCalls++
		if _, err := os.Stat(filepath.Join(opts.CWD, ".deps", "count")); err != nil {
			t.Fatalf("prepared dependencies unavailable during evidence turn: %v", err)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["dependency check"],"testing_summary":"dependencies available","artifacts":[],"scenarios":[{"name":"user runs the configured command","result":"pass","live":true,"evidence":"dependency check passed","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newPreparationTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{
		Prepare: preparationCommand(),
		Test:    dependencyExistsCommand(),
		Lint:    dependencyExistsCommand(),
	})
	sctx.Shared = &pipeline.RunShared{}

	if outcome, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatalf("test step: %v", err)
	} else if outcome.ExitCode != 0 {
		t.Fatalf("test step exit code = %d", outcome.ExitCode)
	}
	if outcome, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatalf("lint step: %v", err)
	} else if outcome.ExitCode != 0 {
		t.Fatalf("lint step exit code = %d", outcome.ExitCode)
	}
	if evidenceCalls != 1 {
		t.Fatalf("evidence agent calls = %d, want 1", evidenceCalls)
	}

	count, err := os.ReadFile(filepath.Join(dir, ".deps", "count"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(count), "prepared"); got != 1 {
		t.Fatalf("prepare executions = %d, want 1; marker=%q", got, count)
	}
}

// A run worktree is a detached linked worktree of the gate, so its committed
// submodules start empty and a repository populates them with
// commands.prepare. Every configured command must see that checkout.
func TestConfiguredTestAndLintSeeSubmoduleCheckedOutByPreparation(t *testing.T) {
	super, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "push", remote, "main")
	gitCmd(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, super, "commit", "-m", "add module")
	headSHA := gitCmd(t, super, "rev-parse", "HEAD")
	gate := filepath.Join(t.TempDir(), "gate.git")
	gitCmd(t, t.TempDir(), "clone", "--bare", super, gate)
	workDir := filepath.Join(t.TempDir(), "run")
	gitCmd(t, gate, "worktree", "add", "--detach", workDir, headSHA)
	if _, err := os.Stat(filepath.Join(workDir, "module", "module.txt")); !os.IsNotExist(err) {
		t.Fatalf("run worktree started with submodule contents: %v", err)
	}

	evidenceSawModule := false
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		_, err := os.Stat(filepath.Join(opts.CWD, "module", "module.txt"))
		evidenceSawModule = err == nil
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["module check"],"testing_summary":"module available","artifacts":[],"scenarios":[{"name":"user runs the configured command","result":"pass","live":true,"evidence":"module check passed","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newPreparationTestContext(t, ag, workDir, baseSHA, headSHA, config.Commands{
		Prepare: "git -c protocol.file.allow=always submodule update --init --recursive",
		Test:    submoduleFileExistsCommand(),
		Lint:    submoduleFileExistsCommand(),
	})
	sctx.GateDir = gate
	sctx.Shared = &pipeline.RunShared{}

	if outcome, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatalf("test step: %v", err)
	} else if outcome.ExitCode != 0 {
		t.Fatalf("test step exit code = %d; the configured command did not see the prepared submodule", outcome.ExitCode)
	}
	if !evidenceSawModule {
		t.Fatal("evidence turn did not see the prepared submodule")
	}
	if outcome, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatalf("lint step: %v", err)
	} else if outcome.ExitCode != 0 {
		t.Fatalf("lint step exit code = %d; the configured command did not see the prepared submodule", outcome.ExitCode)
	}
	if got := gitStatusPorcelain(t, workDir); got != "" {
		t.Fatalf("prepared submodule left superproject changes: %q", got)
	}
}

func TestEnsurePrepared_RemovesNestedRepositoryMutation(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: nestedRepositoryPreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare dependencies: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated")); !os.IsNotExist(err) {
		t.Fatalf("nested repository from preparation survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".deps", "count")); err != nil {
		t.Fatalf("ignored dependency materialization was removed: %v", err)
	}
}

func TestEnsurePrepared_RestoresPendingTrackedAndUntrackedChanges(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("pending staged change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "base.txt")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("pending unstaged change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending_test.go"), []byte("package pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeStatus := gitStatusPorcelain(t, dir)

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare dependencies: %v", err)
	}

	if got := gitStatusPorcelain(t, dir); got != beforeStatus {
		t.Fatalf("pending worktree state after preparation = %q, want %q", got, beforeStatus)
	}
	if got := gitCmd(t, dir, "show", ":base.txt"); got != "pending staged change" {
		t.Fatalf("staged base.txt = %q, want pending staged change", got)
	}
	base, err := os.ReadFile(filepath.Join(dir, "base.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// A reset restores the working tree through Git, which honors its configured
	// checkout line-ending conversion on Windows. The snapshot contract here is
	// the pending text, not Git's platform-specific on-disk representation.
	if strings.ReplaceAll(string(base), "\r\n", "\n") != "pending unstaged change\n" {
		t.Fatalf("working base.txt = %q, want pending unstaged change", base)
	}
	if _, err := os.Stat(filepath.Join(dir, "pending_test.go")); err != nil {
		t.Fatalf("pending untracked file was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prepare.tmp")); !os.IsNotExist(err) {
		t.Fatalf("ordinary untracked preparation artifact survived: %v", err)
	}
}

func TestEnsurePrepared_PreservesSharedStash(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	other := filepath.Join(t.TempDir(), "other")
	gitCmd(t, dir, "worktree", "add", other, "main")
	t.Cleanup(func() { gitCmd(t, dir, "worktree", "remove", "--force", other) })
	if err := os.WriteFile(filepath.Join(other, "unrelated.txt"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "add", "unrelated.txt")
	gitCmd(t, other, "stash", "push", "-m", "unrelated")
	want := gitCmd(t, dir, "rev-parse", "refs/stash")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("pending change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare dependencies: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/stash"); got != want {
		t.Fatalf("shared stash ref = %q, want unrelated stash %q", got, want)
	}
	if got := gitCmd(t, dir, "show", want+":unrelated.txt"); got != "unrelated" {
		t.Fatalf("unrelated stash contents = %q, want preserved payload", got)
	}
}

func TestEnsurePrepared_ResetsRegisteredSubmodule(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "remote", "add", "origin", remote)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "add", ".gitmodules", "module")
	gitCmd(t, dir, "commit", "-m", "add module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	moduleHead := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD")

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: registeredSubmodulePreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare dependencies: %v", err)
	}
	if got := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD"); got != moduleHead {
		t.Fatalf("submodule head after preparation = %q, want %q", got, moduleHead)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("preparation left submodule mutation in parent worktree: %q", got)
	}
}

func TestEnsurePrepared_RestoresDirtyInitializedSubmodule(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "remote", "add", "origin", remote)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "add", ".gitmodules", "module")
	gitCmd(t, dir, "commit", "-m", "add module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	moduleHead := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "module", "module.txt"), []byte("pending before prepare\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeStatus := gitStatusPorcelain(t, dir)

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: dirtySubmodulePreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare with dirty initialized submodule: %v", err)
	}
	if got := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD"); got != moduleHead {
		t.Fatalf("submodule head after preparation = %q, want %q", got, moduleHead)
	}
	if got := readFile(t, filepath.Join(dir, "module", "module.txt")); strings.ReplaceAll(got, "\r\n", "\n") != "pending before prepare\n" {
		t.Fatalf("submodule worktree after preparation = %q, want pending state", got)
	}
	if got := gitStatusPorcelain(t, dir); got != beforeStatus {
		t.Fatalf("parent status after preparation = %q, want %q", got, beforeStatus)
	}
}

func TestEnsurePrepared_DoesNotInitializeUnrelatedSubmodule(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "remote", "add", "origin", remote)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "config", "-f", ".gitmodules", "submodule.module.url", "file:///missing-submodule")
	gitCmd(t, dir, "add", ".gitmodules", "module")
	gitCmd(t, dir, "commit", "-m", "add unavailable module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "submodule", "deinit", "-f", "module")

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: successfulPreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare with uninitialized submodule: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "module", ".git")); !os.IsNotExist(err) {
		t.Fatalf("preparation initialized unavailable submodule: %v", err)
	}
}

func TestEnsurePrepared_KeepsSubmoduleInitializedByPreparationAtRecordedCommit(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "remote", "add", "origin", remote)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "add", ".gitmodules", "module")
	gitCmd(t, dir, "commit", "-m", "add module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "submodule", "deinit", "-f", "module")

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: initializesSubmodulePreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare initializes submodule: %v", err)
	}
	// The configured commands build against the checkout preparation made, so
	// it must survive; the commit the command made inside it must not.
	if got, err := os.ReadFile(filepath.Join(dir, "module", "module.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("submodule checkout after preparation = %q, %v; want recorded module.txt", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "module", "prepared.txt")); !os.IsNotExist(err) {
		t.Fatalf("preparation commit inside the submodule survived: %v", err)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("preparation left submodule mutation: %q", got)
	}
}

// A fix agent's edit inside a submodule cannot be staged by the superproject,
// so the commit leaves it out. The rerun of the configured command must then
// see the recorded submodule tree, not the uncommitted edit.
func TestEnsurePrepared_DiscardsFixAgentEditInsidePreparedSubmodule(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "push", remote, "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "commit", "-m", "add module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "submodule", "deinit", "-f", "module")

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: "git -c protocol.file.allow=always submodule update --init module"})
	sctx.Shared = &pipeline.RunShared{}
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "module", "module.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := commitAgentFixesWithResult(sctx, types.StepTest, "fix tests", "")
	if err != nil {
		t.Fatalf("commit agent fixes: %v", err)
	}
	if committed {
		t.Fatal("superproject committed an edit inside the submodule")
	}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare before rerun: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "module", "module.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("submodule file before rerun = %q, %v; want the recorded content", got, err)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("submodule still differs from the head: %q", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "discarding changes: module") {
		t.Fatalf("discarded submodule edit was not logged: %q", logs)
	}
}

// A file a fix agent creates inside a submodule is untracked there, so neither
// the forced checkout nor the superproject commit removes or carries it. The
// rerun must not see it, while ignored build output inside the submodule
// survives.
func TestEnsurePrepared_DiscardsFixAgentNewFileInsidePreparedSubmodule(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt", ".gitignore")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "push", remote, "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "commit", "-m", "add module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "submodule", "deinit", "-f", "module")

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: "git -c protocol.file.allow=always submodule update --init module"})
	sctx.Shared = &pipeline.RunShared{}
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "module", "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "module", "build", "out.o"), []byte("built\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "module", "missing.h"), []byte("agent file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := commitAgentFixesWithResult(sctx, types.StepTest, "fix tests", "")
	if err != nil {
		t.Fatalf("commit agent fixes: %v", err)
	}
	if committed {
		t.Fatal("superproject committed a new file inside the submodule")
	}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare before rerun: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "module", "missing.h")); !os.IsNotExist(err) {
		t.Fatalf("agent's new submodule file survived before rerun: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "module", "build", "out.o")); err != nil {
		t.Fatalf("ignored submodule build output was removed: %v", err)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("submodule still differs from the head: %q", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "discarding changes: module") {
		t.Fatalf("discarded submodule file was not logged: %q", logs)
	}
}

// A rebase onto a base that bumped a submodule moves the recorded pointer but
// not the populated checkout. The next configured command must run against the
// pointer the rebased head records.
func TestEnsurePrepared_ChecksOutPointerMovedAfterPreparation(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "push", remote, "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "commit", "-m", "add module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "submodule", "deinit", "-f", "module")

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: "git -c protocol.file.allow=always submodule update --init module"})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("bumped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "commit", "-am", "bump module")
	gitCmd(t, seed, "push", remote, "main")
	bumped := gitCmd(t, seed, "rev-parse", "HEAD")
	gitCmd(t, filepath.Join(dir, "module"), "fetch", "origin")
	gitCmd(t, dir, "update-index", "--cacheinfo", "160000,"+bumped+",module")
	gitCmd(t, dir, "commit", "-m", "rebased onto base that bumped module")

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare before rerun: %v", err)
	}
	if got := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD"); got != bumped {
		t.Fatalf("submodule checkout = %s, want the rebased head's pointer %s", got, bumped)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "module", "module.txt")); err != nil || string(got) != "bumped\n" {
		t.Fatalf("submodule file = %q, %v; want the bumped content", got, err)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("submodule still differs from the head: %q", got)
	}
}

func TestEnsurePrepared_RestoresDeletedInitializedSubmodule(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "module.txt")
	gitCmd(t, seed, "commit", "-m", "module base")
	gitCmd(t, seed, "remote", "add", "origin", remote)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", remote, "module")
	gitCmd(t, dir, "add", ".gitmodules", "module")
	gitCmd(t, dir, "commit", "-m", "add module")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	moduleHead := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD")

	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: removesSubmodulePreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare removes submodule: %v", err)
	}
	if got := gitCmd(t, filepath.Join(dir, "module"), "rev-parse", "HEAD"); got != moduleHead {
		t.Fatalf("restored submodule head = %q, want %q", got, moduleHead)
	}
}

func TestEnsurePrepared_RestoresUntrackedModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix executable modes")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	path := filepath.Join(dir, "private", "tool")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tool\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare dependencies: %v", err)
	}
	for _, target := range []string{filepath.Dir(path), path} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o777 {
			t.Fatalf("mode for %s = %#o, want %#o", target, got, 0o777)
		}
	}
}

func TestEnsurePrepared_LogsDurationAfterFailure(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: failingPreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	if err := ensurePrepared(sctx, types.StepTest); err == nil {
		t.Fatal("failing preparation unexpectedly succeeded")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "dependency preparation attempt completed in") {
		t.Fatalf("preparation logs did not include attempt duration: %q", logs)
	}
}

func TestEnsurePrepared_RestoresAfterCleanupTimeout(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("pending change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := gitStatusPorcelain(t, dir)
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	previousTimeout := prepareCleanupTimeout
	previousCleanup := runPreparationCleanup
	prepareCleanupTimeout = time.Millisecond
	runPreparationCleanup = func(ctx context.Context, workDir, head string, submodules []preparationSubmodule) error {
		if err := cleanupPreparationChanges(context.Background(), workDir, head, submodules); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() {
		prepareCleanupTimeout = previousTimeout
		runPreparationCleanup = previousCleanup
	})

	if err := ensurePrepared(sctx, types.StepTest); err == nil {
		t.Fatal("preparation unexpectedly succeeded after cleanup timeout")
	}
	if got := gitStatusPorcelain(t, dir); got != before {
		t.Fatalf("pending worktree state after cleanup timeout = %q, want %q", got, before)
	}
}

func TestEnsurePrepared_IgnoresWorktreeTempDirectory(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	tempDir := filepath.Join(dir, ".tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("pending change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := gitStatusPorcelain(t, dir)
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare with worktree temp directory: %v", err)
	}
	if got := gitStatusPorcelain(t, dir); got != before {
		t.Fatalf("pending worktree state after preparation = %q, want %q", got, before)
	}
}

func TestEnsurePrepared_RestoresEmptyUntrackedDirectories(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	pending := filepath.Join(dir, "pending")
	nested := filepath.Join(pending, "nested")
	empty := filepath.Join(nested, "empty")
	if err := os.MkdirAll(empty, 0o750); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{
			pending: 0o711,
			nested:  0o700,
			empty:   0o750,
		} {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare with empty untracked directory: %v", err)
	}
	for path, mode := range map[string]os.FileMode{
		pending: 0o711,
		nested:  0o700,
		empty:   0o750,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("untracked directory %q was not restored: %v", path, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
			t.Fatalf("untracked directory %q mode = %#o, want %#o", path, info.Mode().Perm(), mode)
		}
	}
}

func TestEnsurePrepared_RestoresNestedInitializedSubmodule(t *testing.T) {
	dir, baseSHA, headSHA := setupNestedSubmodules(t)
	inner := filepath.Join(dir, "outer", "inner")
	innerHead := gitCmd(t, inner, "rev-parse", "HEAD")
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: removesNestedSubmodulePreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare removes nested submodule: %v", err)
	}
	if got := gitCmd(t, inner, "rev-parse", "HEAD"); got != innerHead {
		t.Fatalf("restored nested submodule head = %q, want %q", got, innerHead)
	}
}

func TestEnsurePrepared_KeepsNestedSubmoduleInitializedByPreparation(t *testing.T) {
	dir, baseSHA, headSHA := setupNestedSubmodules(t)
	outer := filepath.Join(dir, "outer")
	gitCmd(t, outer, "submodule", "deinit", "--force", "inner")
	innerGit := filepath.Join(outer, "inner", ".git")
	if _, err := os.Stat(innerGit); !os.IsNotExist(err) {
		t.Fatalf("nested submodule remained initialized before preparation: %v", err)
	}
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: initializesNestedSubmodulePreparationCommand()})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare initializes nested submodule: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outer, "inner", "inner.txt")); err != nil {
		t.Fatalf("preparation discarded the nested submodule checkout: %v", err)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("preparation left nested submodule mutation: %q", got)
	}
}

func TestEnsurePrepared_RestoresIntentToAdd(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "intent.go"), []byte("package intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "--intent-to-add", "intent.go")
	beforeStatus := gitStatusPorcelain(t, dir)
	beforeIndex := gitCmd(t, dir, "ls-files", "--stage", "--debug", "--", "intent.go")
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare with intent-to-add: %v", err)
	}
	if got := gitStatusPorcelain(t, dir); got != beforeStatus {
		t.Fatalf("intent-to-add status after preparation = %q, want %q", got, beforeStatus)
	}
	if got := gitCmd(t, dir, "ls-files", "--stage", "--debug", "--", "intent.go"); got != beforeIndex {
		t.Fatalf("intent-to-add index after preparation = %q, want %q", got, beforeIndex)
	}
}

func TestPreparationSnapshot_RetainsRecoveryData(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{})
	snapshot, err := snapshotPreparationState(sctx.Ctx, dir, sctx.GateDir)
	if err != nil {
		t.Fatalf("snapshot preparation state: %v", err)
	}
	t.Cleanup(snapshot.remove)
	if err := os.Remove(snapshot.repositories[0].indexSnapshot); err != nil {
		t.Fatal(err)
	}
	err = snapshot.restore(context.Background())
	if err == nil {
		t.Fatal("restore unexpectedly succeeded with missing snapshot index")
	}
	if _, statErr := os.Stat(snapshot.dir); statErr != nil {
		t.Fatalf("recovery snapshot was removed: %v", statErr)
	}
	if recoveryErr := preparationRestoreError(snapshot, err); !strings.Contains(recoveryErr.Error(), snapshot.dir) {
		t.Fatalf("recovery error = %q, want snapshot location", recoveryErr)
	}
}

func TestPreparationSnapshot_RejectsGateInsideWorktree(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{})
	gateDir := filepath.Join(dir, "gate")
	gitCmd(t, dir, "init", "--bare", "gate")
	if _, err := snapshotPreparationState(sctx.Ctx, dir, gateDir); err == nil {
		t.Fatal("snapshot accepted a gate directory inside the worktree")
	}
}

func TestPreparationGateInsideWorktree_AcceptsDifferentWindowsVolumes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath volumes are Windows-specific")
	}
	if preparationGateInsideWorktree(`C:\worktree`, `D:\gate`) {
		t.Fatal("gate directory on another volume was treated as inside the worktree")
	}
}

func TestPushStep_PreparesFormatterWithPendingUntrackedChanges(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")
	if err := os.WriteFile(filepath.Join(dir, "pending_test.go"), []byte("package pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Prepare: preparationCommand(),
		Format:  dependencyFormattingCommand(),
	})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, headSHA)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push with pending formatter input: %v", err)
	}
	pushedHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if got := gitCmd(t, upstream, "show", pushedHead+":pending_test.go"); got != "package pending" {
		t.Fatalf("pushed pending test file = %q, want preserved content", got)
	}
	if got := gitCmd(t, upstream, "show", pushedHead+":feature.txt"); !strings.Contains(got, "formatted") {
		t.Fatalf("formatter output missing from pushed feature.txt: %q", got)
	}
}

func ignoreTestDependencies(t *testing.T, dir string) {
	t.Helper()
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte(".deps/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func preparationCommand() string {
	if runtime.GOOS == "windows" {
		return `(if not exist .deps mkdir .deps) & echo prepared>>.deps\count & echo temporary>prepare.tmp & echo changed>base.txt`
	}
	return `mkdir -p .deps && echo prepared >> .deps/count && echo temporary > prepare.tmp && echo changed > base.txt`
}

func dependencyExistsCommand() string {
	if runtime.GOOS == "windows" {
		return `if exist .deps\count (exit /b 0) else (exit /b 1)`
	}
	return `test -f .deps/count`
}

func submoduleFileExistsCommand() string {
	if runtime.GOOS == "windows" {
		return `if exist module\module.txt (exit /b 0) else (exit /b 1)`
	}
	return `test -f module/module.txt`
}

func nestedRepositoryPreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `(if not exist .deps mkdir .deps) & echo prepared>>.deps\count & mkdir generated & git -C generated init & git -C generated config user.name test & git -C generated config user.email test@example.com & echo generated>generated\file.txt & git -C generated add file.txt & git -C generated commit -m generated`
	}
	return `mkdir -p .deps && echo prepared >> .deps/count && mkdir generated && git -C generated init && git -C generated config user.name test && git -C generated config user.email test@example.com && echo generated > generated/file.txt && git -C generated add file.txt && git -C generated commit -m generated`
}

func dependencyFormattingCommand() string {
	if runtime.GOOS == "windows" {
		return `if exist .deps\count (echo formatted>>feature.txt) else (exit /b 1)`
	}
	return `test -f .deps/count && echo formatted >> feature.txt`
}

func successfulPreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `exit /b 0`
	}
	return `true`
}

func registeredSubmodulePreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `git -C module config user.name test & git -C module config user.email test@example.com & echo prepared>module\prepared.txt & git -C module add prepared.txt & git -C module commit -m prepared`
	}
	return `git -C module config user.name test && git -C module config user.email test@example.com && echo prepared > module/prepared.txt && git -C module add prepared.txt && git -C module commit -m prepared`
}

func dirtySubmodulePreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `git -C module config user.name test & git -C module config user.email test@example.com & echo committed>module\module.txt & git -C module add module.txt & git -C module commit -m prepared & echo changed-after-commit>module\module.txt`
	}
	return `git -C module config user.name test && git -C module config user.email test@example.com && echo committed > module/module.txt && git -C module add module.txt && git -C module commit -m prepared && echo changed-after-commit > module/module.txt`
}

func initializesSubmodulePreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `git -c protocol.file.allow=always submodule update --init module & git -C module config user.name test & git -C module config user.email test@example.com & echo prepared>module\prepared.txt & git -C module add prepared.txt & git -C module commit -m prepared`
	}
	return `git -c protocol.file.allow=always submodule update --init module && git -C module config user.name test && git -C module config user.email test@example.com && echo prepared > module/prepared.txt && git -C module add prepared.txt && git -C module commit -m prepared`
}

func removesSubmodulePreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `rmdir /s /q module`
	}
	return `rm -rf module`
}

func removesNestedSubmodulePreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `rmdir /s /q outer\inner`
	}
	return `rm -rf outer/inner`
}

func initializesNestedSubmodulePreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `git -C outer -c protocol.file.allow=always submodule update --init inner`
	}
	return `git -C outer -c protocol.file.allow=always submodule update --init inner`
}

func failingPreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `exit /b 1`
	}
	return `false`
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func newPreparationTestContext(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContext(t, ag, workDir, baseSHA, headSHA, cmds)
	gateDir := t.TempDir()
	gitCmd(t, gateDir, "init", "--bare")
	sctx.GateDir = gateDir
	return sctx
}

func setupNestedSubmodules(t *testing.T) (string, string, string) {
	t.Helper()
	dir, baseSHA, _ := setupGitRepo(t)
	innerRemote := t.TempDir()
	gitCmd(t, innerRemote, "init", "--bare")
	inner := t.TempDir()
	gitCmd(t, inner, "init", "-b", "main")
	gitCmd(t, inner, "config", "user.name", "test")
	gitCmd(t, inner, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(inner, "inner.txt"), []byte("inner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, inner, "add", "inner.txt")
	gitCmd(t, inner, "commit", "-m", "inner base")
	gitCmd(t, inner, "remote", "add", "origin", innerRemote)
	gitCmd(t, inner, "push", "origin", "main")

	outerRemote := t.TempDir()
	gitCmd(t, outerRemote, "init", "--bare")
	outer := t.TempDir()
	gitCmd(t, outer, "init", "-b", "main")
	gitCmd(t, outer, "config", "user.name", "test")
	gitCmd(t, outer, "config", "user.email", "test@example.com")
	gitCmd(t, outer, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", innerRemote, "inner")
	gitCmd(t, outer, "add", ".gitmodules", "inner")
	gitCmd(t, outer, "commit", "-m", "add inner")
	gitCmd(t, outer, "remote", "add", "origin", outerRemote)
	gitCmd(t, outer, "push", "origin", "main")

	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", outerRemote, "outer")
	gitCmd(t, dir, "add", ".gitmodules", "outer")
	gitCmd(t, dir, "commit", "-m", "add outer")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	return dir, baseSHA, headSHA
}

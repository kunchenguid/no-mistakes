package steps

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEnsurePrepared_RunsOnceAndKeepsOnlyIgnoredMaterialization(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
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
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{
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

	count, err := os.ReadFile(filepath.Join(dir, ".deps", "count"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(count), "prepared"); got != 1 {
		t.Fatalf("prepare executions = %d, want 1; marker=%q", got, count)
	}
}

func TestEnsurePrepared_RemovesNestedRepositoryMutation(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: nestedRepositoryPreparationCommand()})
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

	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
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
	if string(base) != "pending unstaged change\n" {
		t.Fatalf("working base.txt = %q, want pending unstaged change", base)
	}
	if _, err := os.Stat(filepath.Join(dir, "pending_test.go")); err != nil {
		t.Fatalf("pending untracked file was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prepare.tmp")); !os.IsNotExist(err) {
		t.Fatalf("ordinary untracked preparation artifact survived: %v", err)
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
		return `if not exist .deps mkdir .deps & echo prepared>>.deps\count & echo temporary>prepare.tmp & echo changed>base.txt`
	}
	return `mkdir -p .deps && echo prepared >> .deps/count && echo temporary > prepare.tmp && echo changed > base.txt`
}

func dependencyExistsCommand() string {
	if runtime.GOOS == "windows" {
		return `if exist .deps\count (exit /b 0) else (exit /b 1)`
	}
	return `test -f .deps/count`
}

func nestedRepositoryPreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `if not exist .deps mkdir .deps & echo prepared>>.deps\count & mkdir generated & git -C generated init & git -C generated config user.name test & git -C generated config user.email test@example.com & echo generated>generated\file.txt & git -C generated add file.txt & git -C generated commit -m generated`
	}
	return `mkdir -p .deps && echo prepared >> .deps/count && mkdir generated && git -C generated init && git -C generated config user.name test && git -C generated config user.email test@example.com && echo generated > generated/file.txt && git -C generated add file.txt && git -C generated commit -m generated`
}

func dependencyFormattingCommand() string {
	if runtime.GOOS == "windows" {
		return `if exist .deps\count (echo formatted>>feature.txt) else (exit /b 1)`
	}
	return `test -f .deps/count && echo formatted >> feature.txt`
}

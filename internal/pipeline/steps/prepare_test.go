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

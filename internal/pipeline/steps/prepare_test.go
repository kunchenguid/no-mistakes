package steps

import (
	"context"
	"fmt"
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
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{
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

func TestEnsurePrepared_PreservesConcurrentSharedStash(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	other := filepath.Join(t.TempDir(), "other")
	gitCmd(t, dir, "worktree", "add", other, "main")
	t.Cleanup(func() { gitCmd(t, dir, "worktree", "remove", "--force", other) })
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("pending change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: concurrentStashPreparationCommand(other)})
	sctx.Shared = &pipeline.RunShared{}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("prepare dependencies: %v", err)
	}
	want := strings.TrimSpace(readFile(t, filepath.Join(dir, ".deps", "unrelated-stash")))
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

func TestEnsurePrepared_DeinitializesSubmoduleInitializedByPreparation(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(dir, "module", ".git")); !os.IsNotExist(err) {
		t.Fatalf("preparation left newly initialized submodule: %v", err)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("preparation left submodule mutation: %q", got)
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

func successfulPreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `exit /b 0`
	}
	return `true`
}

func concurrentStashPreparationCommand(other string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`if not exist .deps mkdir .deps & echo unrelated>"%s\unrelated.txt" & git -C "%s" add unrelated.txt & git -C "%s" stash push -m unrelated & git rev-parse refs/stash>.deps\unrelated-stash`, other, other, other)
	}
	return fmt.Sprintf(`mkdir -p .deps && echo unrelated > %q && git -C %q add unrelated.txt && git -C %q stash push -m unrelated && git rev-parse refs/stash > .deps/unrelated-stash`, filepath.Join(other, "unrelated.txt"), other, other)
}

func registeredSubmodulePreparationCommand() string {
	if runtime.GOOS == "windows" {
		return `git -C module config user.name test & git -C module config user.email test@example.com & echo prepared>module\prepared.txt & git -C module add prepared.txt & git -C module commit -m prepared`
	}
	return `git -C module config user.name test && git -C module config user.email test@example.com && echo prepared > module/prepared.txt && git -C module add prepared.txt && git -C module commit -m prepared`
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

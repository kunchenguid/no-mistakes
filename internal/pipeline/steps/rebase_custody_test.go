package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// detachedRebaseCustodyFixture models the production topology rather than an
// attached clone: the operator worktree stays at the submitted feature head,
// while the pipeline gets a detached linked worktree from the bare gate. Setup
// leaves the gate feature ref at the submitted head; only RebaseStep may move
// it to the rewritten head.
type detachedRebaseCustodyFixture struct {
	ctx       context.Context
	db        *db.DB
	repo      *db.Repo
	run       *db.Run
	sctx      *pipeline.StepContext
	operator  string
	gate      string
	managed   string
	submitted string
}

func newDetachedRebaseCustodyFixture(t *testing.T) *detachedRebaseCustodyFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	gitCmd(t, root, "init", "--bare", upstream)

	operator := filepath.Join(root, "operator")
	gitCmd(t, root, "init", "-b", "main", operator)
	gitCmd(t, operator, "config", "user.name", "test")
	gitCmd(t, operator, "config", "user.email", "test@test.com")
	gitCmd(t, operator, "remote", "add", "origin", upstream)
	if err := os.WriteFile(filepath.Join(operator, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, operator, "add", "base.txt")
	gitCmd(t, operator, "commit", "-m", "base")
	base := gitCmd(t, operator, "rev-parse", "HEAD")
	gitCmd(t, operator, "push", "origin", "main")

	gitCmd(t, operator, "checkout", "-b", "feature/recover")
	if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, operator, "add", "feature.txt")
	gitCmd(t, operator, "commit", "-m", "feature")
	submitted := gitCmd(t, operator, "rev-parse", "HEAD")

	gate := filepath.Join(root, "gate.git")
	gitCmd(t, root, "init", "--bare", gate)
	gitCmd(t, operator, "push", gate,
		"refs/heads/main:refs/heads/main",
		"refs/heads/feature/recover:refs/heads/feature/recover")
	mustStepGitRun(t, ctx, gate, "remote", "add", "origin", upstream)
	mustStepGitRun(t, ctx, gate, "config", "user.name", "test")
	mustStepGitRun(t, ctx, gate, "config", "user.email", "test@test.com")

	// Advance only the upstream default branch after submission. The gate's
	// feature ref deliberately remains at submitted until the real rebase step
	// publishes its rewritten detached HEAD.
	gitCmd(t, operator, "checkout", "main")
	if err := os.WriteFile(filepath.Join(operator, "upstream.txt"), []byte("upstream advance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, operator, "add", "upstream.txt")
	gitCmd(t, operator, "commit", "-m", "upstream advance")
	gitCmd(t, operator, "push", "origin", "main")
	gitCmd(t, operator, "checkout", "feature/recover")

	managed := filepath.Join(root, "managed")
	if err := gitpkg.WorktreeAdd(ctx, gate, managed, submitted); err != nil {
		t.Fatalf("create detached managed worktree: %v", err)
	}
	t.Cleanup(func() { _ = gitpkg.WorktreeRemove(context.Background(), gate, managed) })
	if branch := gitCmd(t, managed, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Fatalf("managed worktree is attached to %q, want detached HEAD", branch)
	}

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, err := database.InsertRepo(operator, upstream, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{
		Ctx: ctx, DB: database, Repo: repo, Run: run, WorkDir: managed,
		Agent: &mockAgent{name: "test"}, Config: &config.Config{},
		Log: func(string) {}, LogChunk: func(string) {}, LogFile: func(string) {},
	}
	return &detachedRebaseCustodyFixture{
		ctx: ctx, db: database, repo: repo, run: run, sctx: sctx,
		operator: operator, gate: gate, managed: managed,
		submitted: submitted,
	}
}

func mustStepGitRun(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := gitpkg.Run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(out)
}

func assertStepNotAncestor(t *testing.T, ctx context.Context, dir, ancestor, descendant string) {
	t.Helper()
	_, err := gitpkg.Run(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected %s not to be an ancestor of %s: %v", ancestor, descendant, err)
	}
}

// TestRebaseStep_DetachedManagedWorktreePublishesRecoverableCustody is the
// production-shaped regression for a push failure after a rebase-only run. No
// fixture advances the gate feature branch: RebaseStep must anchor its detached
// rewritten HEAD before recording it, and the ordinary cleanup must not make
// the terminal head impossible to recover.
func TestRebaseStep_DetachedManagedWorktreePublishesRecoverableCustody(t *testing.T) {
	f := newDetachedRebaseCustodyFixture(t)

	outcome, err := (&RebaseStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("rebase step: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("rebase outcome = %#v", outcome)
	}
	preserved := mustStepGitRun(t, f.ctx, f.managed, "rev-parse", "HEAD")
	if preserved == f.submitted {
		t.Fatal("real rebase did not rewrite the submitted head")
	}
	assertStepNotAncestor(t, f.ctx, f.gate, f.submitted, preserved)
	assertStepNotAncestor(t, f.ctx, f.gate, preserved, f.submitted)
	if got := mustStepGitRun(t, f.ctx, f.gate, "rev-parse", "refs/heads/feature/recover^{commit}"); got != preserved {
		t.Fatalf("gate feature ref = %s, want rebased head %s", got, preserved)
	}
	persisted, err := f.db.GetRun(f.run.ID)
	if err != nil || persisted == nil || persisted.HeadSHA != preserved {
		t.Fatalf("persisted run after rebase = %#v, %v", persisted, err)
	}

	// Model a failure before any successful push, then perform the manager's
	// ordinary detached-worktree cleanup.
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, preserved); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeRemove(f.ctx, f.gate, f.managed); err != nil {
		t.Fatalf("remove managed worktree: %v", err)
	}
	if _, err := os.Stat(f.managed); !os.IsNotExist(err) {
		t.Fatalf("managed worktree still exists after cleanup: %v", err)
	}

	service := &branchsync.Service{DB: f.db, Repo: f.repo, WorkDir: f.operator, GateDir: f.gate}
	plan := service.InspectCached(f.ctx)
	if plan.State != branchsync.StatePipelineOwned || plan.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("anchored custody plan = %#v", plan)
	}
	if plan.NextAction == nil || plan.NextAction.Code != "recover_custody" {
		t.Fatalf("anchored custody next action = %#v", plan.NextAction)
	}

	recovered := service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed || recovered.State != branchsync.StateCustodyReturned {
		t.Fatalf("immediate recovery = %#v", recovered)
	}
	if got := gitCmd(t, f.operator, "rev-parse", "HEAD"); got != preserved {
		t.Fatalf("operator HEAD = %s, want rebased head %s", got, preserved)
	}
	if got := gitCmd(t, f.operator, "rev-parse", "refs/no-mistakes/recover/"+f.run.ID); got != preserved {
		t.Fatalf("recovery anchor = %s, want %s", got, preserved)
	}
}

// TestRebaseStep_DetachedManagedWorktreeConcurrentGateMoveWins pins the CAS
// half of publication: if another gate update lands after the managed worktree
// was carved, the rebase must neither overwrite it nor publish an unanchored
// rewritten SHA into normal run state.
func TestRebaseStep_DetachedManagedWorktreeConcurrentGateMoveWins(t *testing.T) {
	f := newDetachedRebaseCustodyFixture(t)

	writer := filepath.Join(t.TempDir(), "writer")
	gitCmd(t, filepath.Dir(writer), "clone", "--branch", "feature/recover", f.gate, writer)
	gitCmd(t, writer, "config", "user.name", "test")
	gitCmd(t, writer, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(writer, "concurrent.txt"), []byte("concurrent gate movement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, writer, "add", "concurrent.txt")
	gitCmd(t, writer, "commit", "-m", "concurrent gate movement")
	concurrent := gitCmd(t, writer, "rev-parse", "HEAD")
	gitCmd(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")

	_, err := (&RebaseStep{}).Execute(f.sctx)
	if err == nil {
		t.Fatal("rebase publication overwrote or ignored concurrent gate movement")
	}
	rebased := mustStepGitRun(t, f.ctx, f.managed, "rev-parse", "HEAD")
	if rebased == f.submitted {
		t.Fatal("test did not reach a rewritten detached rebase head")
	}
	if got := mustStepGitRun(t, f.ctx, f.gate, "rev-parse", "refs/heads/feature/recover^{commit}"); got != concurrent {
		t.Fatalf("gate ref = %s, want concurrent head %s", got, concurrent)
	}
	persisted, dbErr := f.db.GetRun(f.run.ID)
	if dbErr != nil || persisted == nil {
		t.Fatalf("reload run: %#v, %v", persisted, dbErr)
	}
	if persisted.HeadSHA != f.submitted || f.sctx.Run.HeadSHA != f.submitted {
		t.Fatalf("unanchored rebase was published: db=%s memory=%s want %s", persisted.HeadSHA, f.sctx.Run.HeadSHA, f.submitted)
	}
}

package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRebaseOnlyCancelledRunKeepLocalRecovery reproduces the historical Moxy
// custody gap end to end at the owning package boundary: submit A, let the
// ordinary rebase step create P in a detached gate worktree and record only P
// on the run, cancel before publication, remove the pipeline worktree, then
// return custody while every ordinary branch and PR ref remains exactly A.
func TestRebaseOnlyCancelledRunKeepLocalRecovery(t *testing.T) {
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

	branch := "feature/rebase-custody"
	gitCmd(t, operator, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, operator, "add", "feature.txt")
	gitCmd(t, operator, "commit", "-m", "feature")
	submitted := gitCmd(t, operator, "rev-parse", "HEAD")
	gitCmd(t, operator, "push", "origin", branch)
	gitCmd(t, root, "--git-dir="+upstream, "update-ref", "refs/pull/503/head", submitted)

	// Advance the base only after A is submitted, forcing an ordinary history
	// rewrite when the pipeline rebases the detached gate worktree.
	gitCmd(t, operator, "checkout", "main")
	if err := os.WriteFile(filepath.Join(operator, "main.txt"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, operator, "add", "main.txt")
	gitCmd(t, operator, "commit", "-m", "advance main")
	gitCmd(t, operator, "push", "origin", "main")
	gitCmd(t, operator, "checkout", branch)

	gate := filepath.Join(root, "gate.git")
	gitCmd(t, root, "clone", "--bare", upstream, gate)
	pipelineWorktree := filepath.Join(root, "pipeline")
	gitCmd(t, root, "--git-dir="+gate, "worktree", "add", "--detach", pipelineWorktree, submitted)
	gitCmd(t, pipelineWorktree, "config", "user.name", "test")
	gitCmd(t, pipelineWorktree, "config", "user.email", "test@test.com")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, err := database.InsertRepo(operator, upstream, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, branch, submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := database.ListRunPublicationTargets(run.ID)
	if err != nil || len(targets) == 0 {
		t.Fatalf("publication targets = %#v, %v", targets, err)
	}
	evidence := make([]db.PublicationEvidenceInput, 0, len(targets))
	for _, target := range targets {
		if target.RequestLineage == "" || target.RequestLineage == db.PublicationTargetRequestLineageMigrationPending {
			if err := database.ReconcileRunPublicationTargetLineage(run.ID, target.TargetFingerprint, "none"); err != nil {
				t.Fatalf("reconcile publication lineage: %v", err)
			}
		}
		evidence = append(evidence, db.PublicationEvidenceInput{
			TargetFingerprint: target.TargetFingerprint,
			Ref:               target.Ref,
			TargetVersion:     target.TargetVersion,
			RemoteHash:        "fixture-remote-evidence",
			ProviderHash:      "fixture-provider-evidence",
			Cursor:            "audit-cutoff=1754136000;provider-date:1754136000;audit;hasNextPage=false;fixture-complete-cursor",
			Since:             run.CreatedAt,
			Until:             run.UpdatedAt,
		})
	}
	if _, err := database.RecordRunPublicationEvidence(run.ID, evidence); err != nil {
		t.Fatalf("record publication evidence: %v", err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{
		Ctx: ctx, Run: run, Repo: repo, WorkDir: pipelineWorktree,
		Agent: &mockAgent{name: "test"}, Config: &config.Config{}, DB: database,
		Log: func(string) {}, LogChunk: func(string) {}, LogFile: func(string) {},
	}
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("ordinary rebase step: %v", err)
	}
	preserved := sctx.Run.HeadSHA
	if preserved == submitted {
		t.Fatal("ordinary rebase did not create rewritten head P")
	}
	recorded, err := database.GetRun(run.ID)
	if err != nil || recorded.HeadSHA != preserved {
		t.Fatalf("recorded head after rebase = %#v, %v; want %s", recorded, err, preserved)
	}
	if got := gitCmd(t, root, "--git-dir="+gate, "rev-parse", "refs/heads/"+branch); got != submitted {
		t.Fatalf("rebase moved ordinary gate branch to %s, want A %s", got, submitted)
	}
	if err := database.UpdateRunErrorStatus(run.ID, "cancelled: aborted by user", types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "--git-dir="+gate, "worktree", "remove", "--force", pipelineWorktree)
	gitCmd(t, root, "--git-dir="+gate, "cat-file", "-e", preserved+"^{commit}")

	assertRef := func(label, dir, ref, want string) {
		t.Helper()
		if got := gitCmd(t, dir, "rev-parse", ref); got != want {
			t.Fatalf("%s = %s, want %s", label, got, want)
		}
	}
	assertRef("local head before recovery", operator, "HEAD", submitted)
	if got := gitCmd(t, root, "--git-dir="+gate, "rev-parse", "refs/heads/"+branch); got != submitted {
		t.Fatalf("gate branch before recovery = %s, want %s", got, submitted)
	}
	if got := gitCmd(t, root, "--git-dir="+upstream, "rev-parse", "refs/heads/"+branch); got != submitted {
		t.Fatalf("origin branch before recovery = %s, want %s", got, submitted)
	}
	if got := gitCmd(t, root, "--git-dir="+upstream, "rev-parse", "refs/pull/503/head"); got != submitted {
		t.Fatalf("PR head before recovery = %s, want %s", got, submitted)
	}

	service := &branchsync.Service{DB: database, Repo: repo, WorkDir: operator, GateDir: gate, GateConfigCurrent: func() bool { return true }, InternalMutationConsumed: func(string) error { return nil }}
	state := service.Recover(ctx, true)
	if !state.Recovered || state.Changed || state.State != branchsync.StateCustodyReturned {
		t.Fatalf("keep-local recovery = %#v", state)
	}
	assertRef("recovery anchor", operator, "refs/no-mistakes/recover/"+run.ID, preserved)
	assertRef("local head after recovery", operator, "HEAD", submitted)
	if got := gitCmd(t, root, "--git-dir="+gate, "rev-parse", "refs/heads/"+branch); got != submitted {
		t.Fatalf("gate branch after recovery = %s, want %s", got, submitted)
	}
	if got := gitCmd(t, root, "--git-dir="+upstream, "rev-parse", "refs/heads/"+branch); got != submitted {
		t.Fatalf("origin branch after recovery = %s, want %s", got, submitted)
	}
	if got := gitCmd(t, root, "--git-dir="+upstream, "rev-parse", "refs/pull/503/head"); got != submitted {
		t.Fatalf("PR head after recovery = %s, want %s", got, submitted)
	}
	recorded, err = database.GetRun(run.ID)
	if err != nil || recorded.CustodyReturnedAt == nil {
		t.Fatalf("custody stamp after recovery = %#v, %v", recorded, err)
	}
}

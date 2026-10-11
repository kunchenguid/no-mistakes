package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testPRContextCandidate() PRContextCandidate {
	return PRContextCandidate{
		SourceRepo: "acme/repo", SourceBranch: "feature",
		LocalHeadSHA: strings.Repeat("a", 40), TargetBranch: "main",
		TargetSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("c", 40),
		DiffDigest: strings.Repeat("d", 64),
	}
}

func testPRContextRun(t *testing.T, d *DB) string {
	t.Helper()
	repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.com/acme/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "mutable-head", "untrusted-base")
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func TestRunPRContextInitialAndIdempotentBind(t *testing.T) {
	d := openTestDB(t)
	runID := testPRContextRun(t, d)
	if got, err := d.GetRunPRContext(runID); err != nil || got != nil {
		t.Fatalf("unbound = %+v, %v", got, err)
	}
	candidate := testPRContextCandidate()
	first, err := d.BindRunPRContext(runID, candidate, types.StepRebase)
	if err != nil || !first.Changed || first.Generation != 1 {
		t.Fatalf("first bind = %+v, %v", first, err)
	}
	got, err := d.GetRunPRContext(runID)
	if err != nil || got == nil || got.PRContextCandidate != candidate || got.Generation != 1 || got.ObservedAt <= 0 {
		t.Fatalf("receipt = %+v, %v", got, err)
	}
	again, err := d.BindRunPRContext(runID, candidate, types.StepReview)
	if err != nil || again.Changed || again.Generation != 1 {
		t.Fatalf("identical bind = %+v, %v", again, err)
	}
	gotAgain, err := d.GetRunPRContext(runID)
	if err != nil || *gotAgain != *got {
		t.Fatalf("idempotent receipt changed: before=%+v after=%+v err=%v", got, gotAgain, err)
	}
}

func TestRunPRContextChangedReceiptInvalidatesFromChosenBoundaryIncludingSkipped(t *testing.T) {
	d := openTestDB(t)
	runID := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	steps := map[types.StepName]string{}
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepCI} {
		step, err := d.InsertStepResult(runID, name)
		if err != nil {
			t.Fatal(err)
		}
		steps[name] = step.ID
		status := types.StepStatusCompleted
		if name == types.StepTest {
			status = types.StepStatusSkipped
		}
		if _, err := d.sql.Exec(`UPDATE step_results SET status=?, error='old', completed_at=12, skip_reason='old' WHERE id=?`, status, step.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(runID, candidate.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(steps[types.StepReview], 1, "auto_fix", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunCIReady(runID, true); err != nil {
		t.Fatal(err)
	}
	candidate.TargetSHA = strings.Repeat("e", 40)
	changed, err := d.BindRunPRContext(runID, candidate, types.StepRebase)
	if err != nil || !changed.Changed || changed.Generation != 2 {
		t.Fatalf("changed bind = %+v, %v", changed, err)
	}
	for name, id := range steps {
		step, err := d.GetStepResult(id)
		if err != nil {
			t.Fatal(err)
		}
		if name == types.StepIntent {
			if step.Status != types.StepStatusCompleted {
				t.Fatalf("intent changed: %+v", step)
			}
		} else if step.Status != types.StepStatusPending || step.Error != nil || step.CompletedAt != nil || step.SkipReason != nil {
			t.Fatalf("%s not reset: %+v", name, step)
		}
	}
	run, err := d.GetRun(runID)
	if err != nil || run.ReviewApprovedHeadSHA != nil || run.CIReadyAt != nil {
		t.Fatalf("stale run authority: %+v, %v", run, err)
	}
	rounds, err := d.GetRoundsByStep(steps[types.StepReview])
	if err != nil || len(rounds) != 0 {
		t.Fatalf("superseded review rounds still consume budget: %d, %v", len(rounds), err)
	}
	if err := d.UpdateStepStatus(steps[types.StepRebase], types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	candidate.LocalHeadSHA = strings.Repeat("f", 40)
	changed, err = d.BindRunPRContext(runID, candidate, types.StepReview)
	if err != nil || !changed.Changed || changed.Generation != 3 {
		t.Fatalf("rebase-head bind = %+v, %v", changed, err)
	}
	rebase, err := d.GetStepResult(steps[types.StepRebase])
	if err != nil || rebase.Status != types.StepStatusCompleted {
		t.Fatalf("rebase was reset by own head: %+v, %v", rebase, err)
	}
}

func TestRunPRContextRejectsMalformedAndConflictingIdentity(t *testing.T) {
	d := openTestDB(t)
	runID := testPRContextRun(t, d)
	valid := testPRContextCandidate()
	for _, mutate := range []func(*PRContextCandidate){
		func(c *PRContextCandidate) { c.TargetBranch = " " },
		func(c *PRContextCandidate) { c.LocalHeadSHA = "bad" },
		func(c *PRContextCandidate) { c.DiffDigest = "bad" },
		func(c *PRContextCandidate) { c.PRURL = "https://example.com/pr/1" },
	} {
		bad := valid
		mutate(&bad)
		if _, err := d.BindRunPRContext(runID, bad, types.StepRebase); err == nil {
			t.Fatalf("accepted malformed %+v", bad)
		}
	}
	valid.PRURL = "https://example.com/pr/1"
	valid.SourceRepo = "acme/repo"
	valid.SourceBranch = "feature"
	valid.ForgeHeadSHA = strings.Repeat("a", 40)
	if _, err := d.BindRunPRContext(runID, valid, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	conflict := valid
	conflict.PRURL = "https://example.com/pr/2"
	if _, err := d.BindRunPRContext(runID, conflict, types.StepRebase); err == nil {
		t.Fatal("accepted PR URL retarget")
	}
	conflict = valid
	conflict.SourceBranch = "another"
	if _, err := d.BindRunPRContext(runID, conflict, types.StepRebase); err == nil {
		t.Fatal("accepted source branch retarget")
	}
	if got, err := d.GetRunPRContext(runID); err != nil || got.Generation != 1 || got.PRURL != valid.PRURL {
		t.Fatalf("identity changed: %+v, %v", got, err)
	}
}

func TestRunPRContextRejectsIdentityConflictingWithExistingRunPR(t *testing.T) {
	d := openTestDB(t)
	runID := testPRContextRun(t, d)
	if _, err := d.sql.Exec(`UPDATE runs SET pr_url=? WHERE id=?`, "https://example.com/pr/1", runID); err != nil {
		t.Fatal(err)
	}
	candidate := testPRContextCandidate()
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err == nil {
		t.Fatal("accepted a pre-PR receipt after a PR was already recorded")
	}
	candidate.PRURL = "https://example.com/pr/2"
	candidate.SourceRepo = "acme/repo"
	candidate.SourceBranch = "feature"
	candidate.ForgeHeadSHA = candidate.LocalHeadSHA
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err == nil {
		t.Fatal("accepted a different PR from the run's recorded URL")
	}
	if got, err := d.GetRunPRContext(runID); err != nil || got != nil {
		t.Fatalf("conflicting bind persisted receipt: %+v, %v", got, err)
	}
}

func TestRunPRContextAttachesFirstPRIdentityWithoutResettingSameComparison(t *testing.T) {
	d := openTestDB(t)
	runID := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	review, err := d.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(runID, candidate.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	candidate.PRURL = "https://example.com/pr/1"
	candidate.SourceRepo = "acme/repo"
	candidate.SourceBranch = "feature"
	candidate.ForgeHeadSHA = candidate.LocalHeadSHA
	if _, err := d.sql.Exec(`UPDATE runs SET pr_url=? WHERE id=?`, candidate.PRURL, runID); err != nil {
		t.Fatal(err)
	}
	result, err := d.BindRunPRContext(runID, candidate, types.StepRebase)
	if err != nil || !result.Changed || result.Generation != 2 {
		t.Fatalf("attach identity = %+v, %v", result, err)
	}
	step, err := d.GetStepResult(review.ID)
	if err != nil || step.Status != types.StepStatusCompleted {
		t.Fatalf("review reset on identity-only attach: %+v, %v", step, err)
	}
	run, err := d.GetRun(runID)
	if err != nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != candidate.LocalHeadSHA {
		t.Fatalf("review authority revoked on identity-only attach: %+v, %v", run, err)
	}
}

func TestRunPRContextDiscoveryPersistsPRIdentityWithReceipt(t *testing.T) {
	d := openTestDB(t)
	runID := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	candidate.PRURL = "https://example.com/pr/1"
	candidate.SourceRepo = "acme/repo"
	candidate.SourceBranch = "feature"
	candidate.ForgeHeadSHA = candidate.LocalHeadSHA
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	run, err := d.GetRun(runID)
	if err != nil || run.PRURL == nil || *run.PRURL != candidate.PRURL {
		t.Fatalf("discovered PR URL was not persisted atomically: %+v, %v", run, err)
	}
}

func TestRunPRContextTransactionFailureRollsBackReceiptAndInvalidation(t *testing.T) {
	d := openTestDB(t)
	runID := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(runID, candidate.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER fail_context_invalidation BEFORE UPDATE OF review_approved_head_sha ON runs BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
		t.Fatal(err)
	}
	candidate.TargetSHA = strings.Repeat("e", 40)
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err == nil {
		t.Fatal("bind succeeded despite invalidation error")
	}
	got, err := d.GetRunPRContext(runID)
	if err != nil || got.Generation != 1 || got.TargetSHA == candidate.TargetSHA {
		t.Fatalf("receipt was committed: %+v, %v", got, err)
	}
	step, err = d.GetStepResult(step.ID)
	if err != nil || step.Status != types.StepStatusCompleted {
		t.Fatalf("step was reset: %+v, %v", step, err)
	}
	run, err := d.GetRun(runID)
	if err != nil || run.ReviewApprovedHeadSHA == nil {
		t.Fatalf("review approval was revoked: %+v, %v", run, err)
	}
}

func TestRunPRContextSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runID := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	if _, err := d.BindRunPRContext(runID, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRunPRContext(runID)
	if err != nil || got == nil || got.PRContextCandidate != candidate || got.Generation != 1 {
		t.Fatalf("reopened receipt = %+v, %v", got, err)
	}
	if _, err := d.BindRunPRContext("missing", candidate, types.StepRebase); err == nil || err == sql.ErrNoRows {
		t.Fatalf("missing run bind error = %v", err)
	}
}

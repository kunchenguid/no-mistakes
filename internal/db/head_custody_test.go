package db

import (
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newCompletedLocalOnlyDBFixture(t *testing.T) (*DB, *Repo, *Run, RunHeadRecovery) {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "recorded", "base")
	if err != nil {
		t.Fatal(err)
	}
	submitted := "submitted"
	if _, err := d.sql.Exec(`UPDATE runs SET submitted_head_sha = ?, status = 'completed' WHERE id = ?`, submitted, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range types.AllSteps() {
		step, err := d.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name.Order() <= types.StepLint.Order() {
			if err := d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteStep(step.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		} else if err := d.CompleteStepWithStatus(step.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	a := RunHeadRecovery{
		RunID: run.ID, RepoID: repo.ID, Branch: "feature", BaseSHA: "base",
		ExpectedHeadSHA: "recorded", CandidateHeadSHA: "candidate",
		LocalHeadSHA: submitted, AnchorRef: "refs/no-mistakes/recovery-candidates/" + run.ID + "/candidate",
	}
	return d, repo, run, a
}

func TestAdoptCompletedRunHeadCASTransactionAndFinalCustodyCAS(t *testing.T) {
	d, _, run, authority := newCompletedLocalOnlyDBFixture(t)
	if err := d.AdoptCompletedRunHeadCAS(authority); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetRun(run.ID)
	if got.HeadSHA != authority.CandidateHeadSHA || got.CustodyReturnedAt != nil {
		t.Fatalf("run after head CAS = %#v", got)
	}
	audit, err := d.GetRunHeadRecovery(run.ID)
	if err != nil || audit == nil || !sameRecoveryTuple(*audit, authority) {
		t.Fatalf("audit = %#v, %v", audit, err)
	}
	stamped, err := d.CompleteRunHeadRecoveryCAS(authority)
	if err != nil || !stamped {
		t.Fatalf("custody CAS = %v, %v", stamped, err)
	}
	stamped, err = d.CompleteRunHeadRecoveryCAS(authority)
	if err != nil || stamped {
		t.Fatalf("idempotent custody CAS = %v, %v", stamped, err)
	}
}

func TestAdoptCompletedRunHeadCASRollsBackAuditOnPredicateLoss(t *testing.T) {
	d, _, run, authority := newCompletedLocalOnlyDBFixture(t)
	steps, _ := d.GetStepsByRun(run.ID)
	if err := d.UpdateStepStatus(steps[0].ID, types.StepStatusFailed); err != nil {
		t.Fatal(err)
	}
	err := d.AdoptCompletedRunHeadCAS(authority)
	if !errors.Is(err, ErrRunHeadCAS) {
		t.Fatalf("CAS error = %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.HeadSHA != authority.ExpectedHeadSHA {
		t.Fatalf("head changed to %s", got.HeadSHA)
	}
	if audit, _ := d.GetRunHeadRecovery(run.ID); audit != nil {
		t.Fatalf("audit survived rolled-back CAS: %#v", audit)
	}
}

func TestAdoptCompletedRunHeadCASDenialMatrix(t *testing.T) {
	type mutation struct {
		name  string
		apply func(*DB, *Run, RunHeadRecovery) error
	}
	sqlMutation := func(name, query string, args ...any) mutation {
		return mutation{name: name, apply: func(d *DB, run *Run, _ RunHeadRecovery) error {
			values := make([]any, 0, len(args)+1)
			values = append(values, args...)
			values = append(values, run.ID)
			_, err := d.sql.Exec(query, values...)
			return err
		}}
	}
	mutations := []mutation{
		sqlMutation("pending run", `UPDATE runs SET status = 'pending' WHERE id = ?`),
		sqlMutation("running run", `UPDATE runs SET status = 'running' WHERE id = ?`),
		sqlMutation("failed run", `UPDATE runs SET status = 'failed' WHERE id = ?`),
		sqlMutation("cancelled run", `UPDATE runs SET status = 'cancelled' WHERE id = ?`),
		sqlMutation("run error", `UPDATE runs SET error = 'boom' WHERE id = ?`),
		sqlMutation("awaiting agent", `UPDATE runs SET awaiting_agent_since = 1 WHERE id = ?`),
		sqlMutation("custody stamp", `UPDATE runs SET custody_returned_at = 1 WHERE id = ?`),
		sqlMutation("push active", `UPDATE runs SET push_active = 1 WHERE id = ?`),
		sqlMutation("last pushed head", `UPDATE runs SET last_pushed_sha = 'push' WHERE id = ?`),
		sqlMutation("push target kind", `UPDATE runs SET push_target_kind = 'upstream' WHERE id = ?`),
		sqlMutation("push fingerprint", `UPDATE runs SET push_target_fingerprint = 'fingerprint' WHERE id = ?`),
		sqlMutation("push ref", `UPDATE runs SET push_ref = 'refs/heads/feature' WHERE id = ?`),
		sqlMutation("last pushed timestamp", `UPDATE runs SET last_pushed_at = 1 WHERE id = ?`),
		sqlMutation("push generation", `UPDATE runs SET push_generation = 1 WHERE id = ?`),
		sqlMutation("PR URL", `UPDATE runs SET pr_url = 'https://example.invalid/pr/1' WHERE id = ?`),
		sqlMutation("PR open", `UPDATE runs SET pr_state = 'open' WHERE id = ?`),
		sqlMutation("PR merged", `UPDATE runs SET pr_state = 'merged' WHERE id = ?`),
		sqlMutation("PR closed", `UPDATE runs SET pr_state = 'closed' WHERE id = ?`),
		sqlMutation("PR observation", `UPDATE runs SET pr_state_observed_at = 1 WHERE id = ?`),
		sqlMutation("CI ready", `UPDATE runs SET ci_ready_at = 1 WHERE id = ?`),
		sqlMutation("base changed", `UPDATE runs SET base_sha = 'other-base' WHERE id = ?`),
		sqlMutation("submitted changed", `UPDATE runs SET submitted_head_sha = 'other-local' WHERE id = ?`),
		sqlMutation("review authority changed", `UPDATE runs SET review_approved_head_sha = 'review' WHERE id = ?`),
		{name: "missing step", apply: func(d *DB, run *Run, _ RunHeadRecovery) error {
			_, err := d.sql.Exec(`DELETE FROM step_results WHERE run_id = ? AND step_name = 'intent'`, run.ID)
			return err
		}},
		{name: "duplicate step", apply: func(d *DB, run *Run, _ RunHeadRecovery) error {
			_, err := d.sql.Exec(`INSERT INTO step_results
				(id, run_id, step_name, step_order, status, exit_code, started_at, completed_at)
				VALUES (?, ?, 'intent', 1, 'completed', 0, 1, 1)`, newID(), run.ID)
			return err
		}},
		{name: "unknown step", apply: func(d *DB, run *Run, _ RunHeadRecovery) error {
			_, err := d.sql.Exec(`UPDATE step_results SET step_name = 'unknown' WHERE run_id = ? AND step_name = 'push'`, run.ID)
			return err
		}},
	}
	for _, status := range []types.StepStatus{
		types.StepStatusPending, types.StepStatusRunning, types.StepStatusAwaitingApproval,
		types.StepStatusFixing, types.StepStatusFixReview, types.StepStatusFailed, types.StepStatusSkipped,
	} {
		status := status
		mutations = append(mutations, mutation{name: "prepublication status " + string(status), apply: func(d *DB, run *Run, _ RunHeadRecovery) error {
			_, err := d.sql.Exec(`UPDATE step_results SET status = ? WHERE run_id = ? AND step_name = 'review'`, status, run.ID)
			return err
		}})
	}
	mutations = append(mutations,
		sqlMutation("prepublication exit nonzero", `UPDATE step_results SET exit_code = 1 WHERE step_name = 'test' AND run_id = ?`),
		sqlMutation("prepublication exit missing", `UPDATE step_results SET exit_code = NULL WHERE step_name = 'test' AND run_id = ?`),
		sqlMutation("prepublication start missing", `UPDATE step_results SET started_at = NULL WHERE step_name = 'document' AND run_id = ?`),
		sqlMutation("prepublication completion missing", `UPDATE step_results SET completed_at = NULL WHERE step_name = 'lint' AND run_id = ?`),
		sqlMutation("prepublication error", `UPDATE step_results SET error = 'boom' WHERE step_name = 'intent' AND run_id = ?`),
		sqlMutation("publication completed", `UPDATE step_results SET status = 'completed' WHERE step_name = 'push' AND run_id = ?`),
		sqlMutation("publication running", `UPDATE step_results SET status = 'running' WHERE step_name = 'pr' AND run_id = ?`),
		sqlMutation("publication failed", `UPDATE step_results SET status = 'failed' WHERE step_name = 'ci' AND run_id = ?`),
		sqlMutation("publication exit nonzero", `UPDATE step_results SET exit_code = 1 WHERE step_name = 'push' AND run_id = ?`),
		sqlMutation("publication completion missing", `UPDATE step_results SET completed_at = NULL WHERE step_name = 'pr' AND run_id = ?`),
		sqlMutation("publication error", `UPDATE step_results SET error = 'boom' WHERE step_name = 'ci' AND run_id = ?`),
	)

	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			d, _, run, authority := newCompletedLocalOnlyDBFixture(t)
			if err := tt.apply(d, run, authority); err != nil {
				t.Fatal(err)
			}
			if err := d.AdoptCompletedRunHeadCAS(authority); !errors.Is(err, ErrRunHeadCAS) {
				t.Fatalf("head CAS = %v, want ErrRunHeadCAS", err)
			}
			got, _ := d.GetRun(run.ID)
			if got.HeadSHA != authority.ExpectedHeadSHA {
				t.Fatalf("denial changed head to %s", got.HeadSHA)
			}
			if audit, err := d.GetRunHeadRecovery(run.ID); err != nil || audit != nil {
				t.Fatalf("denial retained audit %#v, %v", audit, err)
			}
		})
	}
}

func TestAdoptCompletedRunHeadCASRejectsSameSecondNewerULID(t *testing.T) {
	d, repo, run, authority := newCompletedLocalOnlyDBFixture(t)
	newer, err := d.InsertRun(repo.ID, run.Branch, "other", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET created_at = ? WHERE id = ?`, run.CreatedAt, newer.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if err := d.AdoptCompletedRunHeadCAS(authority); !errors.Is(err, ErrRunHeadCAS) {
		t.Fatalf("same-second newer CAS = %v", err)
	}
}

func TestCompleteRunHeadRecoveryCASRepeatsFullPredicate(t *testing.T) {
	d, _, run, authority := newCompletedLocalOnlyDBFixture(t)
	if err := d.AdoptCompletedRunHeadCAS(authority); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET pr_url = 'https://example.invalid/pr/1' WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if stamped, err := d.CompleteRunHeadRecoveryCAS(authority); !errors.Is(err, ErrRunHeadCAS) || stamped {
		t.Fatalf("final predicate CAS = %v, %v", stamped, err)
	}
	got, _ := d.GetRun(run.ID)
	if got.CustodyReturnedAt != nil {
		t.Fatal("lost final predicate still stamped custody")
	}
}

func TestCompleteRunHeadRecoveryCASDenialMatrix(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*DB, *Repo, *Run, RunHeadRecovery) error
	}{
		{"status", func(d *DB, _ *Repo, run *Run, _ RunHeadRecovery) error {
			return d.UpdateRunStatus(run.ID, types.RunCancelled)
		}},
		{"publication", func(d *DB, _ *Repo, run *Run, _ RunHeadRecovery) error {
			return d.UpdateRunPRURL(run.ID, "https://example.invalid/pr/1")
		}},
		{"push active", func(d *DB, _ *Repo, run *Run, _ RunHeadRecovery) error { return d.SetRunPushActive(run.ID, true) }},
		{"base", func(d *DB, _ *Repo, run *Run, _ RunHeadRecovery) error {
			_, err := d.sql.Exec(`UPDATE runs SET base_sha = 'other' WHERE id = ?`, run.ID)
			return err
		}},
		{"review authority", func(d *DB, _ *Repo, run *Run, _ RunHeadRecovery) error {
			return d.UpdateRunReviewApprovedHeadSHA(run.ID, "other")
		}},
		{"steps", func(d *DB, _ *Repo, run *Run, _ RunHeadRecovery) error {
			_, err := d.sql.Exec(`UPDATE step_results SET status = 'failed' WHERE run_id = ? AND step_name = 'test'`, run.ID)
			return err
		}},
		{"newer run", func(d *DB, repo *Repo, run *Run, _ RunHeadRecovery) error {
			_, err := d.InsertRun(repo.ID, run.Branch, "newer", run.BaseSHA)
			return err
		}},
		{"audit tuple", func(d *DB, _ *Repo, run *Run, _ RunHeadRecovery) error {
			_, err := d.sql.Exec(`UPDATE run_head_recoveries SET anchor_ref = 'refs/conflict' WHERE run_id = ?`, run.ID)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, repo, run, authority := newCompletedLocalOnlyDBFixture(t)
			if err := d.AdoptCompletedRunHeadCAS(authority); err != nil {
				t.Fatal(err)
			}
			if err := tt.apply(d, repo, run, authority); err != nil {
				t.Fatal(err)
			}
			if stamped, err := d.CompleteRunHeadRecoveryCAS(authority); stamped || !errors.Is(err, ErrRunHeadCAS) {
				t.Fatalf("custody CAS = %v, %v", stamped, err)
			}
			got, _ := d.GetRun(run.ID)
			if got.CustodyReturnedAt != nil {
				t.Fatal("denial stamped custody")
			}
		})
	}
}

func TestAdoptCompletedRunHeadCASDatabaseFailureRollsBackAudit(t *testing.T) {
	d, _, run, authority := newCompletedLocalOnlyDBFixture(t)
	if _, err := d.sql.Exec(`CREATE TRIGGER reject_recovery_head BEFORE UPDATE OF head_sha ON runs
		BEGIN SELECT RAISE(ABORT, 'simulated update failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.AdoptCompletedRunHeadCAS(authority); err == nil || errors.Is(err, ErrRunHeadCAS) {
		t.Fatalf("database failure = %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.HeadSHA != authority.ExpectedHeadSHA {
		t.Fatalf("database failure changed head to %s", got.HeadSHA)
	}
	if audit, err := d.GetRunHeadRecovery(run.ID); err != nil || audit != nil {
		t.Fatalf("database failure retained audit %#v, %v", audit, err)
	}

	closed, _, _, closedAuthority := newCompletedLocalOnlyDBFixture(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.AdoptCompletedRunHeadCAS(closedAuthority); err == nil {
		t.Fatal("closed database unexpectedly accepted head CAS")
	}
}

func TestAdvanceActiveRunHeadCASExactStateAndRetry(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.invalid/live.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "old", "base")
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	a := ActiveRunHeadAdvance{
		RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, StepName: "document",
		ExpectedHead: "old", Candidate: "candidate", AnchorRef: "refs/no-mistakes/run-head-candidates/run/candidate",
	}
	if err := d.AdvanceActiveRunHeadCAS(a); err != nil {
		t.Fatal(err)
	}
	if err := d.AdvanceActiveRunHeadCAS(a); err != nil {
		t.Fatalf("idempotent live retry: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.HeadSHA != a.Candidate {
		t.Fatalf("live head = %s", got.HeadSHA)
	}
}

func TestAdvanceActiveRunHeadCASDenialAndJournalRollback(t *testing.T) {
	for _, tt := range []struct {
		name  string
		apply func(*DB, *Repo, *Run) error
	}{
		{"wrong expected head", func(_ *DB, _ *Repo, _ *Run) error { return nil }},
		{"terminal run", func(d *DB, _ *Repo, run *Run) error { return d.UpdateRunStatus(run.ID, types.RunCompleted) }},
		{"push active", func(d *DB, _ *Repo, run *Run) error { return d.SetRunPushActive(run.ID, true) }},
		{"newer same branch", func(d *DB, repo *Repo, run *Run) error {
			_, err := d.InsertRun(repo.ID, run.Branch, "other", run.BaseSHA)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo(t.TempDir(), "https://example.invalid/live.git", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "old", "base")
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			a := ActiveRunHeadAdvance{RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, StepName: "document", ExpectedHead: "old", Candidate: "candidate", AnchorRef: "refs/candidate"}
			if tt.name == "wrong expected head" {
				a.ExpectedHead = "wrong"
			}
			if err := tt.apply(d, repo, run); err != nil {
				t.Fatal(err)
			}
			if err := d.AdvanceActiveRunHeadCAS(a); !errors.Is(err, ErrRunHeadCAS) {
				t.Fatalf("live CAS = %v", err)
			}
			got, _ := d.GetRun(run.ID)
			if got.HeadSHA != "old" {
				t.Fatalf("live denial changed head to %s", got.HeadSHA)
			}
			var journals int
			if err := d.sql.QueryRow(`SELECT COUNT(*) FROM run_head_advances WHERE run_id = ?`, run.ID).Scan(&journals); err != nil || journals != 0 {
				t.Fatalf("live denial journals = %d, %v", journals, err)
			}
		})
	}
}

func TestRunHeadRecoveryAuditIncludesExactBaseAndReviewAuthority(t *testing.T) {
	d, _, run, authority := newCompletedLocalOnlyDBFixture(t)
	review := "reviewed"
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, review); err != nil {
		t.Fatal(err)
	}
	authority.ReviewApprovedSHA = &review
	if err := d.AdoptCompletedRunHeadCAS(authority); err != nil {
		t.Fatal(err)
	}
	audit, err := d.GetRunHeadRecovery(run.ID)
	if err != nil || audit == nil || audit.BaseSHA != authority.BaseSHA || !sameOptionalString(audit.ReviewApprovedSHA, authority.ReviewApprovedSHA) {
		t.Fatalf("audit authority = %#v, %v", audit, err)
	}
	conflict := authority
	conflict.BaseSHA = "other"
	if err := d.AdoptCompletedRunHeadCAS(conflict); !errors.Is(err, ErrRunHeadCAS) {
		t.Fatalf("conflicting base retry = %v", err)
	}
	conflictingReview := "other-review"
	conflict = authority
	conflict.ReviewApprovedSHA = &conflictingReview
	if err := d.AdoptCompletedRunHeadCAS(conflict); !errors.Is(err, ErrRunHeadCAS) {
		t.Fatalf("conflicting review retry = %v", err)
	}
}

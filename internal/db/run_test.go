package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunInsertAndGet(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if run.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if run.Status != types.RunPending {
		t.Errorf("status = %q, want %q", run.Status, types.RunPending)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Branch != "feature" {
		t.Errorf("branch = %q, want %q", got.Branch, "feature")
	}
	if got.HeadSHA != "abc123" {
		t.Errorf("head sha = %q, want %q", got.HeadSHA, "abc123")
	}
}

func TestRunGetNotFound(t *testing.T) {
	d := openTestDB(t)
	got, err := d.GetRun("nonexistent")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent run")
	}
}

func TestRunsByRepo(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	d.InsertRun(repo.ID, "feature-1", "aaa", "bbb")
	d.InsertRun(repo.ID, "feature-2", "ccc", "ddd")

	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	// newest first
	if runs[0].Branch != "feature-2" {
		t.Errorf("first run branch = %q, want feature-2", runs[0].Branch)
	}
}

func TestActiveRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	// no active run initially
	active, err := d.GetActiveRun(repo.ID, "")
	if err != nil {
		t.Fatalf("get active run: %v", err)
	}
	if active != nil {
		t.Fatal("expected nil active run")
	}

	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	active, _ = d.GetActiveRun(repo.ID, "")
	if active == nil || active.ID != run.ID {
		t.Fatal("expected active run matching inserted run")
	}

	// after completing, no active run
	d.UpdateRunStatus(run.ID, types.RunCompleted)
	active, _ = d.GetActiveRun(repo.ID, "")
	if active != nil {
		t.Fatal("expected nil after completing run")
	}
}

func TestActiveRunPrefersBranch(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/branchpref", "git@github.com:user/branchpref.git", "main")

	// Create two active runs on different branches.
	runA, _ := d.InsertRun(repo.ID, "feature-a", "aaa", "000")
	runB, _ := d.InsertRun(repo.ID, "feature-b", "bbb", "000")

	// Without branch hint, newest (runB) wins.
	active, err := d.GetActiveRun(repo.ID, "")
	if err != nil {
		t.Fatalf("get active run: %v", err)
	}
	if active == nil || active.ID != runB.ID {
		t.Fatalf("expected newest run %q, got %v", runB.ID, active)
	}

	// With branch hint "feature-a", the older matching run wins.
	active, err = d.GetActiveRun(repo.ID, "feature-a")
	if err != nil {
		t.Fatalf("get active run with branch: %v", err)
	}
	if active == nil || active.ID != runA.ID {
		t.Fatalf("expected branch-matching run %q, got %q", runA.ID, active.ID)
	}

	// With branch hint "feature-b", runB is returned.
	active, err = d.GetActiveRun(repo.ID, "feature-b")
	if err != nil {
		t.Fatalf("get active run with branch: %v", err)
	}
	if active == nil || active.ID != runB.ID {
		t.Fatalf("expected branch-matching run %q, got %q", runB.ID, active.ID)
	}

	// With branch hint for a non-existent branch, falls back to newest.
	active, err = d.GetActiveRun(repo.ID, "feature-c")
	if err != nil {
		t.Fatalf("get active run with unknown branch: %v", err)
	}
	if active == nil || active.ID != runB.ID {
		t.Fatalf("expected fallback to newest run %q, got %q", runB.ID, active.ID)
	}
}

func TestUpdateRunStatus(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.Status != types.RunRunning {
		t.Errorf("status = %q, want %q", got.Status, types.RunRunning)
	}
}

func TestUpdateRunPRURL(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	prURL := "https://github.com/user/project/pull/1"
	if err := d.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatalf("update pr url: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.PRURL == nil || *got.PRURL != prURL {
		t.Errorf("pr url = %v, want %q", got.PRURL, prURL)
	}
}

func TestUpdateRunHeadSHA(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	if err := d.UpdateRunHeadSHA(run.ID, "xyz"); err != nil {
		t.Fatalf("update head sha: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.HeadSHA != "xyz" {
		t.Errorf("head sha = %q, want %q", got.HeadSHA, "xyz")
	}
}

func TestUpdateRunError(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	if err := d.UpdateRunError(run.ID, "something broke"); err != nil {
		t.Fatalf("update error: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.Error == nil || *got.Error != "something broke" {
		t.Errorf("error = %v, want %q", got.Error, "something broke")
	}
	if got.Status != types.RunFailed {
		t.Errorf("status = %q, want %q", got.Status, types.RunFailed)
	}
}

func TestCascadeDeleteRepo(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	gotRun, _ := d.GetRun(run.ID)
	if gotRun != nil {
		t.Fatal("expected run to be cascade deleted")
	}
	gotStep, _ := d.GetStepResult(step.ID)
	if gotStep != nil {
		t.Fatal("expected step to be cascade deleted")
	}
}

func TestRecoverStaleRunsMarksRunsFailed(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	// Create runs in various statuses.
	pendingRun, _ := d.InsertRun(repo.ID, "feat-a", "aaa", "bbb")
	runningRun, _ := d.InsertRun(repo.ID, "feat-b", "ccc", "ddd")
	d.UpdateRunStatus(runningRun.ID, types.RunRunning)
	completedRun, _ := d.InsertRun(repo.ID, "feat-c", "eee", "fff")
	d.UpdateRunStatus(completedRun.ID, types.RunCompleted)

	count, err := d.RecoverStaleRuns("daemon crashed")
	if err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}
	if count != 2 {
		t.Errorf("recovered count = %d, want 2", count)
	}

	// Pending and running should be failed.
	got, _ := d.GetRun(pendingRun.ID)
	if got.Status != types.RunFailed {
		t.Errorf("pending run status = %q, want %q", got.Status, types.RunFailed)
	}
	if got.Error == nil || *got.Error != "daemon crashed" {
		t.Errorf("pending run error = %v, want %q", got.Error, "daemon crashed")
	}

	got, _ = d.GetRun(runningRun.ID)
	if got.Status != types.RunFailed {
		t.Errorf("running run status = %q, want %q", got.Status, types.RunFailed)
	}

	// Completed should be untouched.
	got, _ = d.GetRun(completedRun.ID)
	if got.Status != types.RunCompleted {
		t.Errorf("completed run status = %q, want %q", got.Status, types.RunCompleted)
	}
}

func TestRecoverStaleRunsMarksStepsFailed(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project2", "git@github.com:user/project2.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	// Create steps in various statuses.
	runningStep, _ := d.InsertStepResult(run.ID, types.StepReview)
	d.StartStep(runningStep.ID)
	awaitingStep, _ := d.InsertStepResult(run.ID, types.StepTest)
	d.UpdateStepStatus(awaitingStep.ID, types.StepStatusAwaitingApproval)
	fixingStep, _ := d.InsertStepResult(run.ID, types.StepLint)
	d.UpdateStepStatus(fixingStep.ID, types.StepStatusFixing)
	completedStep, _ := d.InsertStepResult(run.ID, types.StepPush)
	d.CompleteStep(completedStep.ID, 0, 100, "/tmp/log")
	pendingStep, _ := d.InsertStepResult(run.ID, types.StepPR)

	_, err := d.RecoverStaleRuns("daemon crashed")
	if err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}

	// Running, awaiting_approval, fixing should be failed.
	for _, tc := range []struct {
		id   string
		name string
		want types.StepStatus
	}{
		{runningStep.ID, "running", types.StepStatusFailed},
		{awaitingStep.ID, "awaiting", types.StepStatusFailed},
		{fixingStep.ID, "fixing", types.StepStatusFailed},
		{completedStep.ID, "completed", types.StepStatusCompleted},
		{pendingStep.ID, "pending", types.StepStatusPending},
	} {
		got, _ := d.GetStepResult(tc.id)
		if got.Status != tc.want {
			t.Errorf("step %s: status = %q, want %q", tc.name, got.Status, tc.want)
		}
	}
}

func TestRecoverStaleRunsNoStaleRuns(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project3", "git@github.com:user/project3.git", "main")

	// Only completed runs.
	run, _ := d.InsertRun(repo.ID, "feat", "abc", "def")
	d.UpdateRunStatus(run.ID, types.RunCompleted)

	count, err := d.RecoverStaleRuns("daemon crashed")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if count != 0 {
		t.Errorf("recovered count = %d, want 0", count)
	}
}

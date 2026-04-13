package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenAndClose(t *testing.T) {
	d := openTestDB(t)
	if d == nil {
		t.Fatal("expected non-nil db")
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	d := openTestDB(t)
	// verify tables exist by querying them
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM repos").Scan(&count); err != nil {
		t.Fatalf("repos table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM runs").Scan(&count); err != nil {
		t.Fatalf("runs table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM step_results").Scan(&count); err != nil {
		t.Fatalf("step_results table missing: %v", err)
	}
}

func TestGetStepResult_LegacyBabysitStepName(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo", "git@github.com:test/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	_, err = d.sql.Exec(
		`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES (?, ?, ?, ?, ?)`,
		"step1", run.ID, "babysit", 7, types.StepStatusPending,
	)
	if err != nil {
		t.Fatalf("insert legacy step result: %v", err)
	}

	step, err := d.GetStepResult("step1")
	if err != nil {
		t.Fatalf("get step result: %v", err)
	}
	if step == nil {
		t.Fatal("expected step result")
	}
	if step.StepName != types.StepCI {
		t.Fatalf("step name = %q, want %q", step.StepName, types.StepCI)
	}
}

func TestRepoInsertAndGet(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if repo.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if repo.WorkingPath != "/home/user/project" {
		t.Errorf("working path = %q, want %q", repo.WorkingPath, "/home/user/project")
	}
	if repo.UpstreamURL != "git@github.com:user/project.git" {
		t.Errorf("upstream url = %q, want %q", repo.UpstreamURL, "git@github.com:user/project.git")
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want %q", repo.DefaultBranch, "main")
	}
	if repo.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}

	got, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil repo")
	}
	if got.ID != repo.ID {
		t.Errorf("id = %q, want %q", got.ID, repo.ID)
	}
}

func TestInsertRepoWithID(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("custom-id-123", "/home/user/myproject", "git@github.com:user/myproject.git", "develop")
	if err != nil {
		t.Fatalf("insert repo with id: %v", err)
	}
	if repo.ID != "custom-id-123" {
		t.Errorf("id = %q, want %q", repo.ID, "custom-id-123")
	}
	if repo.WorkingPath != "/home/user/myproject" {
		t.Errorf("working path = %q, want %q", repo.WorkingPath, "/home/user/myproject")
	}
	if repo.UpstreamURL != "git@github.com:user/myproject.git" {
		t.Errorf("upstream url = %q, want %q", repo.UpstreamURL, "git@github.com:user/myproject.git")
	}
	if repo.DefaultBranch != "develop" {
		t.Errorf("default branch = %q, want %q", repo.DefaultBranch, "develop")
	}
	if repo.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}

	// Verify round-trip through GetRepo.
	got, err := d.GetRepo("custom-id-123")
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got == nil || got.ID != "custom-id-123" {
		t.Fatal("expected repo with custom ID")
	}
	if got.DefaultBranch != "develop" {
		t.Errorf("default branch after get = %q, want %q", got.DefaultBranch, "develop")
	}
}

func TestInsertRepoWithIDDuplicate(t *testing.T) {
	d := openTestDB(t)
	_, err := d.InsertRepoWithID("dup-id", "/path/a", "git@github.com:a/b.git", "main")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Same ID should fail (primary key constraint).
	_, err = d.InsertRepoWithID("dup-id", "/path/b", "git@github.com:c/d.git", "main")
	if err == nil {
		t.Fatal("expected error for duplicate ID")
	}
}

func TestRepoGetByPath(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	got, err := d.GetRepoByPath("/home/user/project")
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if got == nil || got.ID != repo.ID {
		t.Fatalf("expected repo with ID %q", repo.ID)
	}

	got, err = d.GetRepoByPath("/nonexistent")
	if err != nil {
		t.Fatalf("get repo by path (not found): %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent path")
	}
}

func TestRepoGetNotFound(t *testing.T) {
	d := openTestDB(t)
	got, err := d.GetRepo("nonexistent")
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent repo")
	}
}

func TestRepoUniqueWorkingPath(t *testing.T) {
	d := openTestDB(t)
	_, err := d.InsertRepo("/home/user/project", "git@github.com:a/b.git", "main")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err = d.InsertRepo("/home/user/project", "git@github.com:c/d.git", "main")
	if err == nil {
		t.Fatal("expected error for duplicate working_path")
	}
}

func TestRepoDelete(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	got, _ := d.GetRepo(repo.ID)
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}

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

func TestStepInsertAndGet(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if step.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if step.StepName != types.StepReview {
		t.Errorf("step name = %q, want %q", step.StepName, types.StepReview)
	}
	if step.StepOrder != types.StepReview.Order() {
		t.Errorf("step order = %d, want %d", step.StepOrder, types.StepReview.Order())
	}
	if step.Status != types.StepStatusPending {
		t.Errorf("status = %q, want %q", step.Status, types.StepStatusPending)
	}

	got, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.StepName != types.StepReview {
		t.Errorf("step name = %q, want %q", got.StepName, types.StepReview)
	}
}

func TestStepsByRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	// insert in reverse order to verify ordering
	d.InsertStepResult(run.ID, types.StepLint)
	d.InsertStepResult(run.ID, types.StepReview)
	d.InsertStepResult(run.ID, types.StepTest)

	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(steps))
	}
	// should be in execution order
	if steps[0].StepName != types.StepReview {
		t.Errorf("first step = %q, want review", steps[0].StepName)
	}
	if steps[1].StepName != types.StepTest {
		t.Errorf("second step = %q, want test", steps[1].StepName)
	}
	if steps[2].StepName != types.StepLint {
		t.Errorf("third step = %q, want lint", steps[2].StepName)
	}
}

func TestStartStep(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	if err := d.StartStep(step.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	got, _ := d.GetStepResult(step.ID)
	if got.Status != types.StepStatusRunning {
		t.Errorf("status = %q, want %q", got.Status, types.StepStatusRunning)
	}
	if got.StartedAt == nil {
		t.Error("expected non-nil started_at")
	}
}

func TestCompleteStep(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	if err := d.CompleteStep(step.ID, 0, 1500, "/logs/run-1/review.log"); err != nil {
		t.Fatalf("complete step: %v", err)
	}
	got, _ := d.GetStepResult(step.ID)
	if got.Status != types.StepStatusCompleted {
		t.Errorf("status = %q, want %q", got.Status, types.StepStatusCompleted)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", got.ExitCode)
	}
	if got.DurationMS == nil || *got.DurationMS != 1500 {
		t.Errorf("duration = %v, want 1500", got.DurationMS)
	}
	if got.LogPath == nil || *got.LogPath != "/logs/run-1/review.log" {
		t.Errorf("log path = %v, want /logs/run-1/review.log", got.LogPath)
	}
	if got.CompletedAt == nil {
		t.Error("expected non-nil completed_at")
	}
}

func TestFailStep(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	if err := d.FailStep(step.ID, "agent crashed", 1500); err != nil {
		t.Fatalf("fail step: %v", err)
	}
	got, _ := d.GetStepResult(step.ID)
	if got.Status != types.StepStatusFailed {
		t.Errorf("status = %q, want %q", got.Status, types.StepStatusFailed)
	}
	if got.Error == nil || *got.Error != "agent crashed" {
		t.Errorf("error = %v, want %q", got.Error, "agent crashed")
	}
	if got.DurationMS == nil || *got.DurationMS != 1500 {
		t.Errorf("duration_ms = %v, want 1500", got.DurationMS)
	}
}

func TestSetStepFindings(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	findings := `[{"severity":"warning","message":"unused variable"}]`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatalf("set findings: %v", err)
	}
	got, _ := d.GetStepResult(step.ID)
	if got.FindingsJSON == nil || *got.FindingsJSON != findings {
		t.Errorf("findings = %v, want %q", got.FindingsJSON, findings)
	}
}

func TestClearStepFindings(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	findings := `[{"severity":"warning","message":"unused variable"}]`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatalf("set findings: %v", err)
	}
	if err := d.ClearStepFindings(step.ID); err != nil {
		t.Fatalf("clear findings: %v", err)
	}

	got, _ := d.GetStepResult(step.ID)
	if got.FindingsJSON != nil {
		t.Errorf("findings = %v, want nil", got.FindingsJSON)
	}
}

func TestUpdateStepStatus(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	if err := d.UpdateStepStatus(step.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, _ := d.GetStepResult(step.ID)
	if got.Status != types.StepStatusAwaitingApproval {
		t.Errorf("status = %q, want %q", got.Status, types.StepStatusAwaitingApproval)
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

func TestStepRoundInsertAndGet(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"unused var"}],"summary":"1 issue"}`
	r, err := d.InsertStepRound(step.ID, 1, "initial", &findings, 1200)
	if err != nil {
		t.Fatalf("insert round: %v", err)
	}
	if r.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if r.StepResultID != step.ID {
		t.Errorf("step_result_id = %q, want %q", r.StepResultID, step.ID)
	}
	if r.Round != 1 {
		t.Errorf("round = %d, want 1", r.Round)
	}
	if r.Trigger != "initial" {
		t.Errorf("trigger = %q, want %q", r.Trigger, "initial")
	}
	if r.FindingsJSON == nil || *r.FindingsJSON != findings {
		t.Errorf("findings = %v, want %q", r.FindingsJSON, findings)
	}
	if r.DurationMS != 1200 {
		t.Errorf("duration_ms = %d, want 1200", r.DurationMS)
	}
	if r.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}
}

func TestStepRoundNullFindings(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)

	r, err := d.InsertStepRound(step.ID, 1, "initial", nil, 500)
	if err != nil {
		t.Fatalf("insert round: %v", err)
	}
	if r.FindingsJSON != nil {
		t.Errorf("findings = %v, want nil", r.FindingsJSON)
	}
}

func TestGetRoundsByStep(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepLint)

	findings1 := `{"findings":[{"id":"lint-1","severity":"error","description":"missing check"}],"summary":"1 error"}`
	d.InsertStepRound(step.ID, 1, "initial", &findings1, 800)
	d.InsertStepRound(step.ID, 2, "auto_fix", nil, 600)

	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("got %d rounds, want 2", len(rounds))
	}
	if rounds[0].Round != 1 {
		t.Errorf("first round = %d, want 1", rounds[0].Round)
	}
	if rounds[0].Trigger != "initial" {
		t.Errorf("first trigger = %q, want initial", rounds[0].Trigger)
	}
	if rounds[0].FindingsJSON == nil {
		t.Fatal("expected non-nil findings on round 1")
	}
	if rounds[1].Round != 2 {
		t.Errorf("second round = %d, want 2", rounds[1].Round)
	}
	if rounds[1].Trigger != "auto_fix" {
		t.Errorf("second trigger = %q, want auto_fix", rounds[1].Trigger)
	}
	if rounds[1].FindingsJSON != nil {
		t.Errorf("expected nil findings on round 2, got %v", rounds[1].FindingsJSON)
	}
}

func TestGetRoundsByStepEmpty(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepPush)

	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) != 0 {
		t.Errorf("got %d rounds, want 0", len(rounds))
	}
}

func TestStepRoundCascadeDelete(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	d.InsertStepRound(step.ID, 1, "initial", nil, 100)

	// Deleting repo should cascade to runs -> step_results -> step_rounds
	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatalf("get rounds after cascade: %v", err)
	}
	if len(rounds) != 0 {
		t.Errorf("got %d rounds after cascade delete, want 0", len(rounds))
	}
}

func TestOpenCreatesStepRoundsTable(t *testing.T) {
	d := openTestDB(t)
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM step_rounds").Scan(&count); err != nil {
		t.Fatalf("step_rounds table missing: %v", err)
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

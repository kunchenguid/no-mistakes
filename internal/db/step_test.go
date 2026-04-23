package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

func TestCompleteStepWithStatus(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	if err := d.CompleteStepWithStatus(step.ID, types.StepStatusSkipped, 0, 1500, "/logs/run-1/review.log"); err != nil {
		t.Fatalf("complete step with status: %v", err)
	}
	got, _ := d.GetStepResult(step.ID)
	if got.Status != types.StepStatusSkipped {
		t.Errorf("status = %q, want %q", got.Status, types.StepStatusSkipped)
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

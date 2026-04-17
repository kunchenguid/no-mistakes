package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStepRoundInsertAndGet(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"unused var"}],"summary":"1 issue"}`
	r, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1200)
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
	if r.SelectedFindingIDs != nil {
		t.Errorf("expected nil selected_finding_ids on fresh insert, got %v", r.SelectedFindingIDs)
	}
	if r.FixSummary != nil {
		t.Errorf("expected nil fix_summary on non-fix round, got %v", r.FixSummary)
	}
}

func TestStepRoundNullFindings(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)

	r, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 500)
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
	d.InsertStepRound(step.ID, 1, "initial", &findings1, nil, 800)
	fixSummary := "fix missing check"
	d.InsertStepRound(step.ID, 2, "auto_fix", nil, &fixSummary, 600)

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
	if rounds[0].FixSummary != nil {
		t.Errorf("expected nil fix_summary on initial round, got %v", rounds[0].FixSummary)
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
	if rounds[1].FixSummary == nil || *rounds[1].FixSummary != fixSummary {
		t.Errorf("second fix_summary = %v, want %q", rounds[1].FixSummary, fixSummary)
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
	d.InsertStepRound(step.ID, 1, "initial", nil, nil, 100)

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

func TestSetStepRoundSelectedFindingIDs(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"x"},{"id":"review-2","severity":"error","description":"y"}],"summary":"2"}`
	r, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 50)
	if err != nil {
		t.Fatalf("insert round: %v", err)
	}

	selected := `["review-1"]`
	if err := d.SetStepRoundSelectedFindingIDs(r.ID, &selected); err != nil {
		t.Fatalf("set selected: %v", err)
	}

	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("expected 1 round, got %d", len(rounds))
	}
	if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != selected {
		t.Errorf("selected_finding_ids = %v, want %q", rounds[0].SelectedFindingIDs, selected)
	}

	// Clearing the selection resets the column to NULL.
	if err := d.SetStepRoundSelectedFindingIDs(r.ID, nil); err != nil {
		t.Fatalf("clear selected: %v", err)
	}
	rounds, err = d.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if rounds[0].SelectedFindingIDs != nil {
		t.Errorf("expected nil after clear, got %v", rounds[0].SelectedFindingIDs)
	}
}

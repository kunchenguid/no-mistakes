package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_BindsUncertifiedRangeOntoInitialReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, "source-run"); err != nil {
		t.Fatal(err)
	}
	var gotFrom, gotTo, gotSource string
	var fixing bool
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		gotFrom, gotTo, gotSource = sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, sctx.UncertifiedSourceRunID
		fixing = sctx.Fixing
		return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if fixing {
		t.Fatal("initial review ran in fix mode")
	}
	if gotFrom != "from-sha" || gotTo != run.HeadSHA || gotSource != "source-run" {
		t.Fatalf("initial review bound from=%q to=%q source=%q", gotFrom, gotTo, gotSource)
	}
}

func TestBindUncertifiedPipelineRange_CopiesOntoStepContext(t *testing.T) {
	database, _, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, "source-run"); err != nil {
		t.Fatal(err)
	}
	source, err := database.InsertRun(repo.ID, run.Branch, "older", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(source.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"r1","severity":"error","description":"prior bug","action":"auto-fix"}]}`
	if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, source.ID); err != nil {
		t.Fatal(err)
	}

	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: t.TempDir(),
	}
	BindUncertifiedPipelineRange(sctx)
	if sctx.UncertifiedFromSHA != "from-sha" || sctx.UncertifiedToSHA != run.HeadSHA || sctx.UncertifiedSourceRunID != source.ID {
		t.Fatalf("bound range = from=%q to=%q source=%q", sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, sctx.UncertifiedSourceRunID)
	}
	if len(sctx.UncertifiedPriorRounds) != 1 {
		t.Fatalf("prior rounds = %d, want 1", len(sctx.UncertifiedPriorRounds))
	}
}

func TestBindUncertifiedPipelineRange_MissingFromGateWarnsAndContinues(t *testing.T) {
	database, _, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-missing", "to-missing", "source-run"); err != nil {
		t.Fatal(err)
	}
	var logs []string
	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: t.TempDir(),
		Log:     func(line string) { logs = append(logs, line) },
	}
	BindUncertifiedPipelineRange(sctx)
	if sctx.UncertifiedToSHA != "" || sctx.UncertifiedFromSHA != "" {
		t.Fatalf("missing range was applied: from=%q to=%q", sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "uncertified range from-missing..to-missing not in gate; not applying provenance") {
		t.Fatalf("logs = %q, want skip warning", joined)
	}
}

func TestBindUncertifiedPipelineRange_DoesNotBindWhileFixing(t *testing.T) {
	database, _, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, "source-run"); err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{
		Ctx:     context.Background(),
		DB:      database,
		Repo:    repo,
		Run:     run,
		WorkDir: t.TempDir(),
		Fixing:  true,
	}
	BindUncertifiedPipelineRange(sctx)
	if sctx.UncertifiedToSHA != "" {
		t.Fatalf("fixing review bound uncertified range %q", sctx.UncertifiedToSHA)
	}
}

func TestApprovedReview_ClearsUncertifiedRange(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	step := &mockStep{name: types.StepReview, outcome: &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("certified review left uncertified range %#v", got)
	}
}

func TestParkedReview_DoesNotClearUncertifiedRange(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, "from-sha", run.HeadSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	step := &mockStep{
		name: types.StepReview,
		outcome: &StepOutcome{
			NeedsApproval:         true,
			Findings:              `{"findings":[{"id":"r1","severity":"error","description":"fix me","action":"ask-user"}]}`,
			ReviewApprovedHeadSHA: run.HeadSHA,
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	got, err := database.GetUncertifiedPipelineRange(repo.ID, run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ToSHA != run.HeadSHA {
		t.Fatalf("parked review cleared uncertified range: %#v", got)
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	<-done
}

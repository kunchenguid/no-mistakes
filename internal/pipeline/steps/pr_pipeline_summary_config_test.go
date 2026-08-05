package steps

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_PipelineSummaryDisabledOmitsPipelineSection(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	reviewFindings := `{"findings":[],"summary":"clean","risk_level":"medium","risk_rationale":"touches critical error handling"}`
	testRound1 := `{"findings":[{"id":"test-1","severity":"error","file":"pkg/handler_test.go","line":42,"description":"expected 429 got 200"}],"summary":"1 failure"}`

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"fix: improve pipeline header UX","body":"## Summary\n\n- keep branch status readable"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.PipelineSummary = false

	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, reviewFindings); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(reviewStep.ID, 1, "initial", &reviewFindings, nil, 500); err != nil {
		t.Fatal(err)
	}

	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(testStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(testStep.ID, 1, "initial", &testRound1, nil, 800); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(testStep.ID, 2, "auto_fix", nil, nil, 600); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)

	if strings.Contains(ghLog, "## Pipeline") {
		t.Fatalf("expected no Pipeline section with pipeline_summary disabled, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "Updates from [git push no-mistakes]") {
		t.Fatalf("expected no no-mistakes signature with pipeline_summary disabled, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "keep branch status readable") {
		t.Fatalf("expected the What Changed body to survive with pipeline_summary disabled, got:\n%s", ghLog)
	}
}

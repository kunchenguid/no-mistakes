package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_ACarriedStepReportsTheRoundsOfTheRunItWasCarriedFrom(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	failed, err := sctx.DB.InsertRun(sctx.Repo.ID, sctx.Run.Branch, headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	review, err := sctx.DB.InsertStepResult(failed.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 100, ""); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"nil-1","severity":"error","file":"main.go","line":3,"description":"the handler dereferences a nil config"}],"summary":"1 error"}`
	applied := changesAppliedSummary
	if _, err := sctx.DB.InsertStepRound(review.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(review.ID, 2, "auto_fix", nil, &applied, 100); err != nil {
		t.Fatal(err)
	}
	completed, err := sctx.DB.GetStepResult(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertCarriedStepResult(sctx.Run.ID, completed); err != nil {
		t.Fatal(err)
	}

	pipelineMD, _, _ := (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)
	if !strings.Contains(pipelineMD, "the handler dereferences a nil config") || !strings.Contains(pipelineMD, "**Review**") {
		t.Fatalf("pipeline section does not report the carried review's finding and fix:\n%s", pipelineMD)
	}
}

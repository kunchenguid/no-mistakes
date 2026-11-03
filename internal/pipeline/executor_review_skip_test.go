package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ReviewSkipStopsBeforeDownstreamWork(t *testing.T) {
	for _, entry := range []string{"response", "configured", "outcome", "recovered response", "recovered skipped review"} {
		t.Run(entry, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			review := newApprovalStep(types.StepReview, `{"findings":[{"id":"r1","severity":"warning","description":"decline this finding","action":"ask-user"}]}`)
			// A parked candidate is not durable approval authority.
			review.outcome.ReviewApprovedHeadSHA = run.HeadSHA
			testStep, document := newPassStep(types.StepTest), newPassStep(types.StepDocument)
			push := newFailStep(types.StepPush, errors.New("refusing to push: run has no durably recorded review-approved head"))
			steps := []Step{review, testStep, document, push}
			executor := NewExecutor(database, p, nil, nil, steps, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			workDir := t.TempDir()
			switch entry {
			case "configured":
				executor.SetSkippedSteps([]types.StepName{types.StepReview})
				go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
			case "outcome":
				review.outcome = &StepOutcome{Skipped: true, SkipReason: "review unavailable"}
				go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
			case "recovered response", "recovered skipped review":
				gate := types.StepReview
				if entry == "recovered skipped review" {
					gate = types.StepTest
				}
				run = seedReviewSkipRecovery(t, database, run, steps, gate)
				go func() { done <- executor.Resume(ctx, run, repo, workDir) }()
			default:
				go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
			}
			if entry == "response" || entry == "recovered response" {
				respondReviewSkipTest(t, executor, done, types.ActionSkip)
			}
			err := <-done
			if testStep.callCount() != 0 || document.callCount() != 0 || push.callCount() != 0 {
				t.Fatalf("review skip ran downstream work: test=%d document=%d push=%d, error=%v", testStep.callCount(), document.callCount(), push.callCount(), err)
			}
			if err == nil || !strings.Contains(err.Error(), "review") || !strings.Contains(err.Error(), "--action approve") || !strings.Contains(err.Error(), "--ignore") {
				t.Fatalf("error = %v, want a review-skip failure explaining finding dispositions", err)
			}
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if results[0].Status != types.StepStatusFailed || results[0].Error == nil {
				t.Fatalf("review = %+v, want a recorded failure", results[0])
			}
			got, err := database.GetRun(run.ID)
			if err != nil || got.Status != types.RunFailed || got.ReviewApprovedHeadSHA != nil || got.AwaitingAgentSince != nil {
				t.Fatalf("run = %+v, error = %v; want failed without review authority", got, err)
			}
		})
	}
}

func TestExecutor_ReviewSkipLegitimateContinuations(t *testing.T) {
	for _, scenario := range []string{"no publication", "publication skipped", "durable approval", "approve", "ignore", "non-review skip", "pr only", "push skipped", "review and push skipped"} {
		t.Run(scenario, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			review := newApprovalStep(types.StepReview, `{"findings":[{"id":"r1","severity":"warning","description":"optional change","action":"auto-fix"}]}`)
			review.outcome.ReviewApprovedHeadSHA = run.HeadSHA
			testStep, document := newPassStep(types.StepTest), newPassStep(types.StepDocument)
			lint := newPassStep(types.StepLint)
			push, pr := newPassStep(types.StepPush), newPassStep(types.StepPR)
			steps := []Step{review, testStep, document, lint, push, pr}
			if scenario == "no publication" {
				steps = steps[:4]
			}
			if scenario == "pr only" {
				steps = []Step{review, testStep, document, lint, pr}
			}
			if scenario == "durable approval" {
				// Intentionally leave the caller's Run stale. The DB owns authority.
				if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, run.HeadSHA); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "ignore" {
				reviewStep := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
					if sctx.Fixing {
						return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}, nil
					}
					return review.outcome, nil
				}}
				steps[0] = reviewStep
			}
			if scenario == "non-review skip" {
				review.outcome.NeedsApproval = false
				review.outcome.Findings = ""
				testStep.outcome.NeedsApproval = true
			}
			executor := NewExecutor(database, p, nil, nil, steps, nil)
			if scenario == "publication skipped" {
				executor.SetSkippedSteps([]types.StepName{types.StepPush, types.StepPR})
			}
			if scenario == "push skipped" {
				executor.SetSkippedSteps([]types.StepName{types.StepPush})
			}
			if scenario == "review and push skipped" {
				executor.SetSkippedSteps([]types.StepName{types.StepReview, types.StepPush})
			}
			done, _ := startExecutor(t, executor, run, repo, t.TempDir())
			action := types.ActionSkip
			if scenario == "approve" {
				action = types.ActionApprove
			}
			if scenario == "ignore" {
				action = types.ActionFix
			}
			if scenario == "non-review skip" {
				waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
				if err := executor.Respond(types.StepTest, types.ActionSkip, nil); err != nil {
					t.Fatal(err)
				}
			} else if scenario != "review and push skipped" {
				respondReviewSkipTest(t, executor, done, action)
				if scenario == "ignore" {
					respondReviewSkipTest(t, executor, done, types.ActionApprove)
				}
			}
			waitExecutorDone(t, done)
			if testStep.callCount() != 1 || document.callCount() != 1 || lint.callCount() != 1 {
				t.Fatal("legitimate continuation did not execute downstream steps")
			}
			if scenario == "pr only" || scenario == "push skipped" || scenario == "review and push skipped" {
				if push.callCount() != 0 || pr.callCount() != 1 {
					t.Fatalf("continuation without Push: push=%d pr=%d", push.callCount(), pr.callCount())
				}
			}
		})
	}
}

func respondReviewSkipTest(t *testing.T, executor *Executor, done <-chan error, action types.ApprovalAction) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var err error
		if action == types.ActionFix {
			_, err = executor.RespondWithOverrides(types.StepReview, action, nil, []string{"r1"}, nil, nil, "")
		} else {
			err = executor.Respond(types.StepReview, action, nil)
		}
		if err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("executor ended before response: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("review response: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func seedReviewSkipRecovery(t *testing.T, database *db.DB, run *db.Run, steps []Step, gate types.StepName) *db.Run {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		sr, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		if step.Name() == types.StepReview && gate != types.StepReview {
			if err := database.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
		}
		if step.Name() != gate {
			continue
		}
		findings := `{"findings":[{"id":"r1","severity":"warning","description":"decision","action":"ask-user"}]}`
		if err := database.StartStep(sr.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, run.HeadSHA, 10); err != nil {
			t.Fatal(err)
		}
		if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 10, &findings); err != nil {
			t.Fatal(err)
		}
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

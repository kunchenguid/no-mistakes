package pipeline

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestScopedResponseRefusesUnknownMalformedAndWrongRunBeforeMutation(t *testing.T) {
	for _, which := range []string{"legacy", "malformed", "missing-skip", "wrong-run", "executor-mismatch"} {
		t.Run(which, func(t *testing.T) {
			database, p, r, _ := setupTest(t)
			skips := []types.StepName{types.StepPush, types.StepPR, types.StepCI}
			if which != "legacy" {
				if err := database.SetRunSkippedSteps(r.ID, skips); err != nil {
					t.Fatal(err)
				}
			}
			if which == "malformed" {
				control, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				if _, err := control.Exec(`UPDATE runs SET skip_steps = 'null' WHERE id = ?`, r.ID); err != nil {
					t.Fatal(err)
				}
			}
			sr, err := database.InsertStepResult(r.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			e := NewExecutor(database, p, nil, nil, nil, nil)
			e.SetSkippedSteps(skips)
			if which == "executor-mismatch" {
				e.SetSkippedSteps(nil)
			}
			e.waiting = true
			e.waitingStep = types.StepReview
			e.waitingStepResultID = sr.ID
			opt := ResponseOptions{RequiredSkips: skips, ScopedRunID: r.ID}
			if which == "wrong-run" {
				opt.ScopedRunID = "another-run"
			}
			if which == "missing-skip" {
				opt.RequiredSkips = append(opt.RequiredSkips, types.StepTest)
			}
			for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix} {
				if _, err := e.RespondWithOverrides(types.StepReview, action, nil, nil, nil, nil, "", opt); err == nil {
					t.Fatal("unsafe response accepted")
				}
				if !e.waiting || len(e.approvalCh) != 0 {
					t.Fatal("refusal released the gate")
				}
				got, err := database.GetStepResult(sr.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.ResponseOrigin != types.ResponseOriginUnknown {
					t.Fatalf("refusal wrote origin: %s", got.ResponseOrigin)
				}
				rounds, err := database.GetRoundsByStep(sr.ID)
				if err != nil || len(rounds) != 0 {
					t.Fatalf("refusal wrote decision: %v %v", rounds, err)
				}
			}
		})
	}
}

func TestExecutorMalformedPolicyExecutesNothing(t *testing.T) {
	database, p, r, repo := setupTest(t)
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`UPDATE runs SET skip_steps = '{broken' WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	review := newPassStep(types.StepReview)
	e := NewExecutor(database, p, nil, nil, []Step{review}, nil)
	if err := e.Execute(context.Background(), r, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), "malformed skip policy") {
		t.Fatalf("Execute: %v", err)
	}
	if err := e.Resume(context.Background(), r, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), "malformed skip policy") {
		t.Fatalf("Resume: %v", err)
	}
	if review.callCount() != 0 {
		t.Fatal("corrupt policy ran Review")
	}
}

func TestExecutorPolicyPersistenceFailureExecutesNothingAndTerminalizes(t *testing.T) {
	database, p, r, repo := setupTest(t)
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TRIGGER deny_policy BEFORE UPDATE OF skip_steps ON runs
		BEGIN SELECT RAISE(FAIL, 'policy write failed'); END`); err != nil {
		t.Fatal(err)
	}
	review := newPassStep(types.StepReview)
	events := &eventCollector{}
	e := NewExecutor(database, p, nil, nil, []Step{review}, events.handler)
	e.SetSkippedSteps([]types.StepName{types.StepPush})
	if err := e.Execute(context.Background(), r, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), "policy write failed") {
		t.Fatalf("Execute: %v", err)
	}
	if review.callCount() != 0 {
		t.Fatal("a step ran before policy was durable")
	}
	assertDurableFailedRunAndEvent(t, database, r.ID, events)
}

func TestExecutorValidationRestartRetainsDurableSkips(t *testing.T) {
	database, p, r, repo := setupTest(t)
	skips := []types.StepName{types.StepRebase, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	if err := database.SetRunSkippedSteps(r.ID, skips); err != nil {
		t.Fatal(err)
	}
	var reviewCalls, restarts int
	var steps []Step
	for _, name := range types.AllSteps() {
		if name == types.StepReview {
			steps = append(steps, &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) { reviewCalls++; return &StepOutcome{}, nil }})
			steps = append(steps, &adaptiveCallStep{name: types.CustomGateStepName(types.StepReview, "restart"), fn: func(*StepContext) (*StepOutcome, error) {
				restarts++
				if restarts == 1 {
					return &StepOutcome{RestartFrom: types.StepReview}, nil
				}
				return &StepOutcome{}, nil
			}})
		} else {
			steps = append(steps, &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
				if name != types.StepIntent {
					t.Errorf("forbidden step %s ran", name)
				}
				return &StepOutcome{}, nil
			}})
		}
	}
	e := NewExecutor(database, p, nil, nil, steps, nil)
	if err := e.Execute(context.Background(), r, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 2 || restarts != 2 {
		t.Fatalf("review calls=%d restart calls=%d", reviewCalls, restarts)
	}
}

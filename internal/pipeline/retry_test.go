package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInvocationFailureAgentMarksOnlyALiveInvocationsOwnError(t *testing.T) {
	providerErr := errors.New("claude exited with status 1: provider processing error")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		err    error
		marked bool
	}{
		{name: "provider error", ctx: context.Background(), err: providerErr, marked: true},
		{name: "success", ctx: context.Background()},
		{name: "context already ended", ctx: cancelled, err: providerErr},
		{name: "cancellation", ctx: context.Background(), err: fmt.Errorf("run: %w", context.Canceled)},
		{name: "deadline", ctx: context.Background(), err: fmt.Errorf("run: %w", context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &fallbackUsageAgent{name: "inner", err: tc.err}
			_, err := (&invocationFailureAgent{inner: inner}).Run(tc.ctx, agent.RunOpts{})
			if got := errors.Is(err, ErrAgentInvocationFailed); got != tc.marked {
				t.Fatalf("marked = %v, want %v (err=%v)", got, tc.marked, err)
			}
			if tc.err != nil && (err == nil || err.Error() != tc.err.Error()) {
				t.Fatalf("error text changed: got %v, want %v", err, tc.err)
			}
		})
	}
}

func TestExecutor_RecordsAnAgentInvocationFailureAsTheStepsCause(t *testing.T) {
	providerErr := errors.New("codex exited with status 1: usage limit reached")
	callAgent := func(sctx *StepContext) (*StepOutcome, error) {
		if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review"}); err != nil {
			return nil, fmt.Errorf("review agent: %w", err)
		}
		return &StepOutcome{}, nil
	}
	hang := func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for _, tc := range []struct {
		name  string
		agent agent.Agent
		step  func(*StepContext) (*StepOutcome, error)
		cause bool
	}{
		{name: "direct agent call", agent: &fallbackUsageAgent{name: "codex", err: providerErr}, step: callAgent, cause: true},
		{name: "budgeted agent seam", agent: &fallbackUsageAgent{name: "codex", err: providerErr}, cause: true, step: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"}); err != nil {
				return nil, fmt.Errorf("review agent: %w", err)
			}
			return &StepOutcome{}, nil
		}},
		{name: "agent timeout", agent: &hangingAgent{name: "hang", runFn: hang}, step: callAgent},
		{name: "the step's own verdict", agent: &fallbackUsageAgent{name: "codex", result: &agent.Result{}}, step: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := callAgent(sctx); err != nil {
				return nil, err
			}
			return nil, errors.New("tests failed")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := &adaptiveCallStep{name: types.StepReview, fn: tc.step}
			cfg := &config.Config{AgentTimeout: 50 * time.Millisecond}
			if err := NewExecutor(database, p, cfg, tc.agent, []Step{step}, nil).Execute(context.Background(), run, repo, t.TempDir()); err == nil {
				t.Fatal("execute succeeded, want the step to fail")
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := steps[0].FailureCause != nil && *steps[0].FailureCause == db.StepFailureCauseAgentInvocation
			if steps[0].Status != types.StepStatusFailed || got != tc.cause {
				t.Fatalf("step status=%s agent-invocation cause=%v, want failed with cause=%v (error %v)", steps[0].Status, got, tc.cause, steps[0].Error)
			}
		})
	}
}

// failedSourceRun records a run that failed at failedStep on an agent
// invocation error: every step before it completed, every step after it never
// ran, except those named in skipped.
func failedSourceRun(t *testing.T, database *db.DB, repo *db.Repo, steps []types.StepName, failedStep types.StepName, skipped ...types.StepName) *db.Run {
	t.Helper()
	run, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	failedAt := -1
	for i, name := range steps {
		sr, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		isSkipped := false
		for _, s := range skipped {
			isSkipped = isSkipped || s == name
		}
		switch {
		case isSkipped:
			err = database.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, "")
		case name == failedStep:
			failedAt = i
			err = database.FailStepWithCause(sr.ID, "agent exited", 10, db.StepFailureCauseAgentInvocation)
		case failedAt < 0:
			err = database.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 10, "")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunErrorStatus(run.ID, "step failed", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	return run
}

func stepsNamed(names ...types.StepName) []Step {
	steps := make([]Step, len(names))
	for i, name := range names {
		steps[i] = newPassStep(name)
	}
	return steps
}

func TestPlanRetryCheckpointRefusesAnUnprovenBoundary(t *testing.T) {
	core := []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument}
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, database *db.DB, run *db.Run) []Step
		want   string
	}{
		{name: "provable", want: ""},
		{name: "cause not recorded", want: "did not fail on an agent invocation error", mutate: func(t *testing.T, database *db.DB, run *db.Run) []Step {
			steps, _ := database.GetStepsByRun(run.ID)
			if err := database.FailStep(steps[1].ID, "review agent: findings unreadable", 10); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "a second failed step", want: "both failed", mutate: func(t *testing.T, database *db.DB, run *db.Run) []Step {
			steps, _ := database.GetStepsByRun(run.ID)
			if err := database.FailStepWithCause(steps[3].ID, "agent exited", 1, db.StepFailureCauseAgentInvocation); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "a step ran after the failure", want: "after the failed step", mutate: func(t *testing.T, database *db.DB, run *db.Run) []Step {
			steps, _ := database.GetStepsByRun(run.ID)
			if err := database.CompleteStepWithStatus(steps[2].ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "a step before the failure never completed", want: "before the failed step", mutate: func(t *testing.T, database *db.DB, run *db.Run) []Step {
			steps, _ := database.GetStepsByRun(run.ID)
			if err := database.UpdateStepStatus(steps[0].ID, types.StepStatusRunning); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "no step failed", want: "no step failed", mutate: func(t *testing.T, database *db.DB, run *db.Run) []Step {
			steps, _ := database.GetStepsByRun(run.ID)
			for _, step := range steps[1:] {
				if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}},
		{name: "step sequence changed", want: "but the pipeline's is", mutate: func(t *testing.T, database *db.DB, run *db.Run) []Step {
			return stepsNamed(types.StepIntent, types.StepReview, types.StepTest, types.StepDocument)
		}},
		{name: "step count changed", want: "recorded 4 steps but its pipeline has 5", mutate: func(t *testing.T, database *db.DB, run *db.Run) []Step {
			return stepsNamed(append(core, types.StepLint)...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, _, _, repo := setupTest(t)
			run := failedSourceRun(t, database, repo, core, types.StepReview)
			steps := stepsNamed(core...)
			if tc.mutate != nil {
				if replaced := tc.mutate(t, database, run); replaced != nil {
					steps = replaced
				}
			}
			checkpoint, err := PlanRetryCheckpoint(database, run, steps)
			if tc.want == "" {
				if err != nil || checkpoint.Index != 1 || checkpoint.Failed().StepName != types.StepReview || checkpoint.Round != 1 {
					t.Fatalf("checkpoint = %+v, err = %v; want review at index 1, round 1", checkpoint, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestPlanRetryCheckpointNeverCarriesTestOrALaterStep(t *testing.T) {
	core := []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument}
	database, _, _, repo := setupTest(t)
	atTest := failedSourceRun(t, database, repo, core, types.StepTest)
	if checkpoint, err := PlanRetryCheckpoint(database, atTest, stepsNamed(core...)); err != nil || checkpoint.Failed().StepName != types.StepTest {
		t.Fatalf("a failure at Test must be retryable: checkpoint=%+v err=%v", checkpoint, err)
	}
	afterTest := failedSourceRun(t, database, repo, core, types.StepDocument)
	_, err := PlanRetryCheckpoint(database, afterTest, stepsNamed(core...))
	if err == nil || !strings.Contains(err.Error(), "cannot carry test") {
		t.Fatalf("err = %v, want a refusal to carry Test", err)
	}
}

func TestExecutor_RetryCarriesTheStepsBeforeTheFailedOneAndRestartsIt(t *testing.T) {
	database, p, _, repo := setupTest(t)
	names := []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint}
	source := failedSourceRun(t, database, repo, names, types.StepTest, types.StepDocument)
	sourceSteps, err := database.GetStepsByRun(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(sourceSteps[1].ID, 1, "initial", nil, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(source.ID, "reviewed"); err != nil {
		t.Fatal(err)
	}

	continuation, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	steps := make([]*mockStep, len(names))
	pipelineSteps := make([]Step, len(names))
	for i, name := range names {
		steps[i] = newPassStep(name)
		pipelineSteps[i] = steps[i]
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, pipelineSteps, nil)
	exec.SetRetrySource(source.ID)
	if err := exec.Execute(context.Background(), continuation, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	for i, want := range []int{0, 0, 1, 0, 1} {
		if got := steps[i].callCount(); got != want {
			t.Errorf("%s executed %d times, want %d", names[i], got, want)
		}
	}
	rows, err := database.GetStepsByRun(continuation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows[:2] {
		if row.Status != types.StepStatusCompleted || row.CarriedFrom == nil || *row.CarriedFrom != sourceSteps[i].ID || row.RoundsStepID() != sourceSteps[i].ID {
			t.Errorf("%s = %+v, want completed and carried from %s", row.StepName, row, sourceSteps[i].ID)
		}
	}
	if rows[3].Status != types.StepStatusSkipped {
		t.Errorf("document = %s, want the source run's skip kept", rows[3].Status)
	}
	rounds, err := database.GetRoundsByStep(rows[2].ID)
	if err != nil || len(rounds) != 1 || rounds[0].Trigger != "retry" {
		t.Fatalf("test rounds = %+v (err %v), want one retry round", rounds, err)
	}
	if lint, _ := database.GetRoundsByStep(rows[4].ID); len(lint) != 1 || lint[0].Trigger != "initial" {
		t.Errorf("lint rounds = %+v, want one initial round", lint)
	}
	got, err := database.GetRun(continuation.ID)
	if err != nil || got.Status != types.RunCompleted || got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != "reviewed" {
		t.Fatalf("continuation = %+v (err %v), want completed with the carried review approval", got, err)
	}
}

func TestExecutor_RetryFailsClosedWhenTheSourceNoLongerMatchesThePipeline(t *testing.T) {
	database, p, _, repo := setupTest(t)
	source := failedSourceRun(t, database, repo, []types.StepName{types.StepRebase, types.StepReview, types.StepTest}, types.StepReview)
	continuation, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	review := newPassStep(types.StepReview)
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{newPassStep(types.StepRebase), review, newPassStep(types.StepTest), newPassStep(types.StepDocument)}, nil)
	exec.SetRetrySource(source.ID)
	err = exec.Execute(context.Background(), continuation, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "retry run "+source.ID) {
		t.Fatalf("execute = %v, want a refusal naming the source run", err)
	}
	if review.callCount() != 0 {
		t.Fatalf("review executed %d times after the plan was refused", review.callCount())
	}
	if rows, _ := database.GetStepsByRun(continuation.ID); len(rows) != 0 {
		t.Fatalf("refused continuation recorded %d steps", len(rows))
	}
}

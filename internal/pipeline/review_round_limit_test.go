package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ReviewFixRoundLimitIncludesExplicitFixes(t *testing.T) {
	database, p, run, repo := setupTest(t)
	repoConfig, err := config.LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(&config.GlobalConfig{}, repoConfig)
	parks := make(chan string, 4)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, AutoFixable: true,
			Findings: `{"findings":[{"id":"R1","severity":"error","file":"main.go","description":"still broken","action":"auto-fix"}]}`}, nil
	}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil &&
			(*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
			parks <- *event.Findings
		}
	})
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitReviewBudgetPark(t, parks, done)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"R1"}); err != nil {
		t.Fatal(err)
	}
	findings := waitReviewBudgetPark(t, parks, done)
	if !strings.Contains(findings, "still broken") {
		t.Fatalf("remaining finding disappeared: %s", findings)
	}
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"R1"}); err == nil || !strings.Contains(err.Error(), "fix_rounds_exhausted") {
		t.Fatalf("second fix should be refused without releasing the gate, got %v", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

func waitReviewBudgetPark(t *testing.T, parks <-chan string, done <-chan error) string {
	t.Helper()
	select {
	case raw := <-parks:
		return raw
	case err := <-done:
		t.Fatalf("executor ended instead of parking: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("executor did not park")
	}
	return ""
}

const roundLimitFinding = `{"findings":[{"id":"R1","severity":"error","file":"main.go","description":"still broken","action":"auto-fix"}]}`

func TestExecutor_ReviewFixRoundLimitAutomaticAndMixed(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		step                    types.StepName
		auto, limit, firstFixes int
		wantExhausted           bool
	}{
		{"automatic", types.StepReview, 3, 1, 1, true},
		{"mixed", types.StepReview, 1, 2, 1, false},
		{"unbounded", types.StepReview, 3, 0, 3, false},
		{"other-step", types.StepLint, 3, 1, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			cfg := &config.Config{Review: config.Review{MaxFixRounds: tc.limit}, AutoFix: config.AutoFix{Review: tc.auto, Lint: tc.auto}}
			parks := make(chan string, 4)
			step := &adaptiveCallStep{name: tc.step, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: roundLimitFinding}, nil
			}}
			exec := NewExecutor(database, p, cfg, nil, []Step{step}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusFixReview) {
					parks <- *event.Findings
				}
			})
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())
			raw := waitReviewBudgetPark(t, parks, done)
			if HasReviewFixRoundLimit(raw) != tc.wantExhausted {
				t.Fatalf("exhaustion marker=%v, want %v: %s", HasReviewFixRoundLimit(raw), tc.wantExhausted, raw)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := database.GetRoundsByStep(steps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := reviewFixRoundCount(rounds); got != tc.firstFixes {
				t.Fatalf("fix rounds=%d, want %d", got, tc.firstFixes)
			}
			if !tc.wantExhausted {
				if err := exec.Respond(tc.step, types.ActionFix, []string{"R1"}); err != nil {
					t.Fatal(err)
				}
				raw = waitReviewBudgetPark(t, parks, done)
				if tc.name == "mixed" && !HasReviewFixRoundLimit(raw) {
					t.Fatalf("automatic and explicit fixes did not share the cap: %s", raw)
				}
			}
			if err := exec.Respond(tc.step, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			waitExecutorDone(t, done)
		})
	}
}

func TestExecutor_ReviewFixRoundLimitWithProtectedPathRefusalOffersOnlySkipOrAbort(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{Review: config.Review{MaxFixRounds: 1}, AutoFix: config.AutoFix{Review: 1}}
	parks := make(chan string, 4)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: roundLimitFinding}, nil
		}
		return nil, &ProtectedPathError{Path: "package.lock", Rule: "*.lock"}
	}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil &&
			(*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
			parks <- *event.Findings
		}
	})
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	findings := waitReviewBudgetPark(t, parks, done)
	if !HasProtectedPathRefusal(findings) || !HasReviewFixRoundLimit(findings) {
		t.Fatalf("refused last fix round should park with both markers: %s", findings)
	}

	approveErr := exec.Respond(types.StepReview, types.ActionApprove, nil)
	if approveErr == nil {
		t.Fatal("approve must stay refused at a protected-path refusal")
	}
	_, fixErr := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{protectedPathFindingID}, nil, nil, nil, "")
	var fixRefusal *RespondRefusal
	if !errors.As(fixErr, &fixRefusal) || !strings.Contains(fixRefusal.Message, "fix_rounds_exhausted") {
		t.Fatalf("fix must be refused by the exhausted budget, got %v", fixErr)
	}
	for name, text := range map[string]string{"approve refusal": approveErr.Error(), "fix refusal help": fixRefusal.Help} {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "use fix") || strings.Contains(lower, "approve, skip") || !strings.Contains(lower, "skip") || !strings.Contains(lower, "abort") {
			t.Errorf("%s must name only skip or abort, the responses this gate accepts: %q", name, text)
		}
	}

	if err := exec.Respond(types.StepReview, types.ActionSkip, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

func TestExecutor_ReviewFixRoundLimitCleanFinalReviewPasses(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{Review: config.Review{MaxFixRounds: 1}, AutoFix: config.AutoFix{Review: 3}}
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: roundLimitFinding}, nil
		}
		return &StepOutcome{ReviewedPaths: []string{"main.go"}, ReviewablePaths: []string{"main.go"}}, nil
	}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.Execute(ctx, run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusCompleted || steps[0].FindingsJSON != nil {
		t.Fatalf("clean final review should complete without a budget finding: %+v", steps[0])
	}
}

func TestExecutor_ReviewFixRoundLimitRecoveryKeepsPin(t *testing.T) {
	one, two, zero := 1, 2, 0
	for _, tc := range []struct {
		pin       *int
		config    int
		exhausted bool
	}{
		{&one, 9, true}, {&two, 1, false}, {&zero, 1, false}, {nil, 1, false},
	} {
		t.Run(fmt.Sprintf("pin-%v-config-%d", tc.pin, tc.config), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			raw := roundLimitFinding
			if tc.exhausted {
				var err error
				raw, err = appendReviewFixRoundLimit(raw, 1, *tc.pin)
				if err != nil {
					t.Fatal(err)
				}
			}
			sr, recovered := seedRecoveredReviewGate(t, database, run, raw, types.StepStatusFixReview, `["R1"]`)
			if tc.pin != nil {
				if err := database.PinReviewMaxFixRounds(sr.ID, *tc.pin); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.InsertReviewStepRound(sr.ID, 2, "auto_fix", &raw, nil, "reviewed-head", 1); err != nil {
				t.Fatal(err)
			}
			// Answer and coverage continuations are not additional fixes.
			for i, trigger := range []string{"answer", "coverage"} {
				if _, err := database.InsertReviewStepRound(sr.ID, i+3, trigger, &raw, nil, "reviewed-head", 1); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{Review: config.Review{MaxFixRounds: tc.config}}
			parks := make(chan string, 4)
			step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{NeedsApproval: true, Findings: roundLimitFinding, ReviewApprovedHeadSHA: "reviewed-head"}, nil
			}}
			exec := NewExecutor(database, p, cfg, nil, []Step{step}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusFixReview) {
					parks <- *event.Findings
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			work := t.TempDir()
			go func() { done <- exec.Resume(ctx, recovered, repo, work) }()
			t.Cleanup(func() { cancel() })
			waitReviewBudgetPark(t, parks, done)
			err := exec.Respond(types.StepReview, types.ActionFix, []string{"R1"})
			if tc.exhausted {
				if err == nil || !strings.Contains(err.Error(), "fix_rounds_exhausted") {
					t.Fatalf("recovered cap bypassed: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				waitReviewBudgetPark(t, parks, done)
			}
			if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			waitExecutorDone(t, done)
		})
	}
}

func TestExecutor_ReviewFixRoundLimitAnswerCanVerifyTheFinalFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	raw, err := appendReviewFixRoundLimit(roundLimitFinding, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	sr, recovered := seedRecoveredReviewGate(t, database, run, raw, types.StepStatusFixReview, `["R1"]`)
	if err := database.PinReviewMaxFixRounds(sr.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(sr.ID, 2, "auto_fix", &raw, nil, "reviewed-head", 1); err != nil {
		t.Fatal(err)
	}
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.FinalizingAnswers || !sctx.SkipFixExecution {
			return nil, fmt.Errorf("answer should resume the reviewer without another fix")
		}
		if HasReviewFixRoundLimit(sctx.CarriedFindings) {
			return nil, fmt.Errorf("budget notice must not become a carried code finding")
		}
		return &StepOutcome{ReviewedPaths: []string{"main.go"}, ReviewablePaths: []string{"main.go"}, ReviewApprovedHeadSHA: "reviewed-head"}, nil
	}}
	parks := make(chan string, 1)
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusFixReview) {
			parks <- *event.Findings
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	work := t.TempDir()
	go func() { done <- exec.Resume(ctx, recovered, repo, work) }()
	waitReviewBudgetPark(t, parks, done)
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
	got, err := database.GetStepResult(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FindingsJSON != nil || got.Status != types.StepStatusCompleted {
		t.Fatalf("verified final fix remained parked on a budget notice: %+v", got)
	}
}

func TestReviewFixRoundCountExcludesNonFixTurns(t *testing.T) {
	rounds := []*db.StepRound{}
	for _, trigger := range []string{"initial", "auto_fix", "user_fix", "answer", "coverage", "retry"} {
		rounds = append(rounds, &db.StepRound{Trigger: trigger})
	}
	if got := reviewFixRoundCount(rounds); got != 2 {
		t.Fatalf("fix count=%d, want 2", got)
	}
}

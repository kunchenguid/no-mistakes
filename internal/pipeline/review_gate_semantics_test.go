package pipeline

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Regressions for the 2026-11-25 pingap review stall (run
// 01M89BRYDGWCYSGK6001738C86). The scout's two reproductions pinned the stall
// mechanisms; these are the same journeys with INVERTED assertions, so each
// fails against the pre-fix behavior:
//
//  1. An explicit --ignore of an ask-user finding is a durable resolution: a
//     clean rereview completes the run instead of re-parking on the declined
//     finding for ever (dropDeclinedAskUserFindings).
//  2. A verified-fixed finding clears even when the rereview reports a
//     different-line sibling in the same file: the gate tracks live defects,
//     not a monotonically growing carry (narrowed resolveVerifiedFindingsJSON
//     veto).

// waitForReviewRounds waits until the review step has persisted n rounds. A
// round row is inserted before its gate parks, so this is a race-free way to
// observe that the round after a response actually ran.
func waitForReviewRounds(t *testing.T, database *db.DB, runID string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) > 0 {
			rounds, err := database.GetRoundsByStep(steps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) >= n {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("review did not reach %d rounds", n)
}

func TestReviewGate_DeclinedAskUserFindingIsDurablyResolved(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var round atomic.Int32
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if round.Add(1) == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings: `{"findings":[` +
						`{"id":"r1","severity":"error","file":"service.go","line":10,"description":"nil deref","action":"auto-fix"},` +
						`{"id":"r9","severity":"warning","file":"service.go","line":20,"description":"simplification: remove the flag?","action":"ask-user"}],` +
						`"summary":"2 findings"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
			return &StepOutcome{
				Findings:        `{"findings":[],"summary":"clean"}`,
				ReviewedPaths:   []string{"service.go"},
				ReviewablePaths: []string{"service.go"},
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"r1"}, []string{"r9"}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	// The clean rereview verifies r1 and the recorded decline resolves r9:
	// the run COMPLETES. Pre-fix, the gate re-parked on r9 after every clean
	// round and only an explicit approve could ever finish the run.
	waitExecutorDone(t, done)

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("review status = %s, want completed", steps[0].Status)
	}
	if steps[0].FindingsJSON != nil && strings.Contains(*steps[0].FindingsJSON, `"r9"`) {
		t.Fatalf("declined ask-user finding stayed in the live gate: %s", *steps[0].FindingsJSON)
	}

	// The decline stays on the record: the round history derives it from the
	// selection complement, so later prompts still see "declined", never a
	// silent disappearance.
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !HasDeclinedAskUserFinding(gateFindingsWithR9, rounds) {
		t.Fatal("the recorded decline of r9 is not derivable from the round history")
	}
}

const gateFindingsWithR9 = `{"findings":[` +
	`{"id":"r1","severity":"error","file":"service.go","line":10,"description":"nil deref","action":"auto-fix"},` +
	`{"id":"r9","severity":"warning","file":"service.go","line":20,"description":"simplification: remove the flag?","action":"ask-user"}],` +
	`"summary":"2 findings"}`

// An omitted finding is never declined: the response is refused and nothing is
// resolved (the issue #790 safety rule the durable decline must not weaken).
func TestReviewGate_OmissionNeverResolvesAnAskUserFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{NeedsApproval: true, Findings: gateFindingsWithR9,
				ReviewedPaths: []string{"service.go"}, ReviewablePaths: []string{"service.go"}}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	_, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"r1"}, nil, nil, nil, "")
	var refusal *RespondRefusal
	if err == nil || !strings.Contains(err.Error(), "r9") {
		t.Fatalf("omitting r9 must be refused naming it, got %v", err)
	}
	if !asRespondRefusal(err, &refusal) {
		t.Fatalf("expected a RespondRefusal, got %T", err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil || !strings.Contains(*steps[0].FindingsJSON, `"r9"`) {
		t.Fatal("a refused response resolved the omitted finding")
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

func asRespondRefusal(err error, target **RespondRefusal) bool {
	return errors.As(err, target)
}

func TestReviewGate_HotFileRoundClearsVerifiedFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var round atomic.Int32
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			switch round.Add(1) {
			case 1:
				return &StepOutcome{
					NeedsApproval: true,
					Findings: `{"findings":[` +
						`{"id":"r1","severity":"error","file":"service.go","line":10,"description":"defect one","action":"auto-fix"},` +
						`{"id":"r2","severity":"warning","file":"service.go","line":30,"description":"defect two","action":"auto-fix"}],` +
						`"summary":"2 findings"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			case 2:
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"n1","severity":"warning","file":"service.go","line":50,"description":"new sibling defect","action":"auto-fix"}],"summary":"1 finding"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			default:
				return &StepOutcome{
					NeedsApproval:   true,
					Findings:        `{"findings":[{"id":"n2","severity":"warning","file":"service.go","line":70,"description":"another new sibling","action":"auto-fix"}],"summary":"1 finding"}`,
					ReviewedPaths:   []string{"service.go"},
					ReviewablePaths: []string{"service.go"},
				}, nil
			}
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"r1", "r2"}, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	waitForReviewRounds(t, database, run.ID, 2)

	gateIDs := func() ([]string, string) {
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		gate := *steps[0].FindingsJSON
		parsed, err := types.ParseFindingsJSON(gate)
		if err != nil {
			t.Fatal(err)
		}
		return findingIDs(parsed.Items), gate
	}

	// r1/r2 were covered and not re-reported; n1 is a different-line sibling.
	// Pre-fix the blanket same-file veto carried all THREE findings.
	if ids, gate := gateIDs(); len(ids) != 1 || ids[0] != "n1" {
		t.Fatalf("gate after round 2 = %v, want only the live sibling n1: %s", ids, gate)
	}

	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"n1"}, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	waitForReviewRounds(t, database, run.ID, 3)

	// Pre-fix the gate grew monotonically to 4 with only n2 live.
	if ids, gate := gateIDs(); len(ids) != 1 || ids[0] != "n2" {
		t.Fatalf("gate after round 3 = %v, want only the live sibling n2: %s", ids, gate)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// The default cap parks with the remaining findings after 3 fix rounds and
// never approves or drops them; an explicit trusted 0 stays unlimited.
func TestReviewGate_DefaultFixRoundCapParksWithoutApproving(t *testing.T) {
	database, p, run, repo := setupTest(t)
	// The production path: an absent trusted key resolves through
	// EffectiveRepoConfig, exactly as the daemon builds a run's config.
	cfg := config.Merge(&config.GlobalConfig{}, config.EffectiveRepoConfig(nil, nil, false))
	if cfg.Review.MaxFixRounds != config.DefaultReviewMaxFixRounds {
		t.Fatalf("default budget = %d, want %d", cfg.Review.MaxFixRounds, config.DefaultReviewMaxFixRounds)
	}
	var round atomic.Int32
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			n := round.Add(1)
			return &StepOutcome{
				NeedsApproval: true,
				Findings: `{"findings":[{"id":"f` + strconv.Itoa(int(n)) + `","severity":"warning","file":"service.go","line":` + strconv.Itoa(int(n)*10) +
					`,"description":"fresh defect each round","action":"auto-fix"}],"summary":"1 finding"}`,
				ReviewedPaths:   []string{"service.go"},
				ReviewablePaths: []string{"service.go"},
			}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())

	// Initial park, then three operator-dispatched fix rounds (auto_fix.review
	// defaults to 0). Each rereview reports a NEW different-line defect, so the
	// loop would run for ever without the cap.
	for i := range config.DefaultReviewMaxFixRounds {
		if i == 0 {
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
		} else {
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
		}
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
		if err != nil {
			t.Fatal(err)
		}
		var live []string
		for _, item := range parsed.Items {
			if !IsReservedFindingID(item.ID) {
				live = append(live, item.ID)
			}
		}
		if len(live) == 0 {
			t.Fatalf("park %d carries no live finding", i+1)
		}
		// The DB status can lead the executor's gate registration by a moment;
		// retry until the gate accepts the response.
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, live, nil, nil, nil, "")
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("fix round %d refused before the cap: %v", i+1, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The third rereview leaves a finding: the gate parks with the exhaustion
	// notice and the remaining finding, still undecided.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	var gate string
	deadline := time.Now().Add(5 * time.Second)
	for {
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if steps[0].FindingsJSON != nil && HasReviewFixRoundLimit(*steps[0].FindingsJSON) {
			gate = *steps[0].FindingsJSON
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cap reached but the gate never carried the exhaustion notice")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !HasReviewFixRoundLimit(gate) {
		t.Fatalf("cap reached but the gate carries no exhaustion notice: %s", gate)
	}
	if !strings.Contains(gate, "fresh defect each round") {
		t.Fatalf("the remaining finding was dropped at the cap: %s", gate)
	}
	if run, err := database.GetRun(run.ID); err != nil {
		t.Fatal(err)
	} else if run.Status == types.RunCompleted {
		t.Fatal("the cap auto-approved the run")
	}

	// Further fixes are refused without releasing the gate; only an explicit
	// terminal action ends the run.
	for {
		err := exec.Respond(types.StepReview, types.ActionFix, []string{"f4"})
		if err != nil && strings.Contains(err.Error(), "fix_rounds_exhausted") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fix past the cap must be refused with fix_rounds_exhausted, got %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		err := exec.Respond(types.StepReview, types.ActionApprove, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal approve refused: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitExecutorDone(t, done)
}

// A crash between recording the decline and the next round must not resurrect
// the declined ask-user finding: recovery seeds the outstanding set from the
// parked payload, and the carry-forward drop applies on the resumed round.
func TestReviewGate_RecoveryDoesNotResurrectADeclinedAskUserFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	findings := `{"findings":[` +
		`{"id":"review-1","severity":"error","file":"service.go","line":10,"description":"selected issue","action":"ask-user"},` +
		`{"id":"review-9","severity":"warning","file":"service.go","line":20,"description":"keep the flag?","action":"ask-user"}],"summary":"2 findings"}`
	stepResult, recoveredRun := seedRecoveredReviewGate(t, database, run, findings, types.StepStatusAwaitingApproval, "")
	// Record the accepted-but-never-dispatched decision: fix review-1, decline
	// review-9 (the decline is the complement of the selection).
	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds = %v, %v", rounds, err)
	}
	selected := `["review-1"]`
	if err := database.SetStepRoundUserDecision(rounds[0].ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}

	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{
			Findings:        `{"findings":[],"summary":"clean"}`,
			ReviewedPaths:   []string{"service.go"},
			ReviewablePaths: []string{"service.go"},
		}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Resume(ctx, recoveredRun, repo, t.TempDir()) }()

	// Re-answer the recovered gate: the recorded selection is restored into
	// the dispatch, and the recorded decline of review-9 rides the round.
	deadline := time.Now().Add(5 * time.Second)
	var respondErr error
	for time.Now().Before(deadline) {
		if _, respondErr = exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-1"}, []string{"review-9"}, nil, nil, ""); respondErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("respond to recovered gate: %v", respondErr)
	}

	// The recorded decline resolves review-9 on the resumed round: the clean
	// rereview completes the run without another operator decision on it.
	waitExecutorDone(t, done)
	got, err := database.GetStepResult(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.StepStatusCompleted {
		t.Fatalf("recovered review status = %s, want completed", got.Status)
	}
}

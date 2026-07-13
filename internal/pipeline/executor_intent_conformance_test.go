package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This is the forensic §5 reproduction with the final assertion flipped to the
// FIXED behavior. An explicit, authoritative intent forbids retry-only and
// requires a guarded removal; the initial review raises an error/auto-fix race
// finding; the fixer "resolves" it by deleting the required behavior. Before
// the fix the intent-contradicting auto-fix completed silently. After the fix,
// the rereview's conformance obligation surfaces the contradiction as an
// ask-user finding, and one ask-user finding parks the run with no executor
// change (executor.go gate). The step is modeled with a scripted step whose
// rereview turn returns what the fixed ReviewStep returns on a contradiction.
//
// The park is observable as the step reaching fix_review with the run's
// awaiting-agent marker set; the run row itself stays "running" while a gate is
// open (there is no separate awaiting-approval run status), so the assertions
// key off the step status and the marker, not the run status.
func TestExecutor_AutoFixContradictingIntentParksForApproval(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Persisted, resolved intent: removal is REQUIRED, retry-only is REJECTED,
	// and it is authoritative (Source=="agent"), as `axi run --intent` stamps.
	intent := "REQUIRED: on packed-refs.lock, retry then guarded removal of a " +
		"provably-stale lock. REJECTED: retry-only. FORBIDDEN: a cleanup mutex."
	if err := database.UpdateRunIntent(run.ID, db.RunIntent{Summary: intent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatalf("persist intent: %v", err)
	}
	run.Intent = &intent
	source := db.RunIntentSourceAgent
	run.IntentSource = &source

	// review auto-fix ON, as in the incident.
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	call := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			call++
			// The executor must propagate provenance so a step can tell an
			// authoritative intent from an inferred hint.
			if sctx.IntentSource != db.RunIntentSourceAgent {
				t.Errorf("call %d: IntentSource = %q, want %q", call, sctx.IntentSource, db.RunIntentSourceAgent)
			}
			if call == 1 {
				// Initial review: a correctness finding, classified auto-fix.
				return &StepOutcome{
					AutoFixable:   true,
					NeedsApproval: true,
					Findings: `{"findings":[{"severity":"error","action":"auto-fix",` +
						`"description":"unlink can race a live lock; avoid automatic unlinking"}],"risk_level":"high"}`,
				}, nil
			}
			// Post-fix rereview: the fixer resolved the finding by deleting the
			// required removal (retry-only). The conformance obligation flags
			// the contradiction as an ask-user finding even though retry-only
			// is otherwise risk-clean.
			if !sctx.Fixing {
				t.Errorf("call %d: expected Fixing to be true on rereview", call)
			}
			return &StepOutcome{
				Findings: `{"findings":[{"severity":"error","action":"ask-user",` +
					`"description":"fix deletes the intent-required guarded removal, leaving rejected retry-only"}],"risk_level":"high"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	// The run must PARK at fix_review (an ask-user finding after a fix cycle),
	// not silently complete.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	if call != 2 {
		t.Errorf("expected 2 calls (initial + one auto-fix rereview), got %d", call)
	}

	// The awaiting-agent marker confirms the run parked at the gate rather than
	// completing through it.
	got, _ := database.GetRun(run.ID)
	if got.AwaitingAgentSince == nil {
		t.Error("expected run to be parked awaiting the agent, but awaiting_agent_since is nil")
	}
	if got.Status == types.RunCompleted {
		t.Error("expected the intent-contradicting auto-fix to park, but the run completed")
	}

	// Resolve so the executor goroutine exits cleanly.
	exec.Respond(types.StepReview, types.ActionApprove, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

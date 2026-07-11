package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var intentConformanceLineageRE = regexp.MustCompile(`lineage (\S+), severity`)

type intentConformanceAgent struct {
	intent       string
	initialCalls int
	fixerCalls   int
	verifyCalls  int
}

func (a *intentConformanceAgent) Name() string { return "codex" }
func (a *intentConformanceAgent) Close() error { return nil }

func (a *intentConformanceAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	switch {
	case strings.Contains(opts.Prompt, "initial routed review"):
		a.initialCalls++
		return &agent.Result{Output: []byte(
			`{"findings":[{"severity":"error","action":"auto-fix",` +
				`"description":"unlink can race a live lock; avoid automatic unlinking"}],"risk_level":"high"}`,
		)}, nil
	case strings.Contains(opts.Prompt, "Fix the following"):
		a.fixerCalls++
		if !strings.Contains(opts.Prompt, a.intent) {
			return nil, fmt.Errorf("fixer prompt omitted explicit user intent")
		}
		return &agent.Result{Output: []byte(`{"summary":"replace guarded removal with retry-only"}`)}, nil
	case strings.Contains(opts.Prompt, "Independently verify whether"):
		a.verifyCalls++
		matches := intentConformanceLineageRE.FindStringSubmatch(opts.Prompt)
		if len(matches) != 2 {
			return nil, fmt.Errorf("verifier prompt omitted finding lineage")
		}
		lineageID := matches[1]
		return &agent.Result{Output: []byte(fmt.Sprintf(
			`{"verdicts":[{"lineage_id":%q,"status":"resolved","rationale":"the race is gone"}],`+
				`"new_findings":[{"description":"fix deletes the intent-required guarded removal, leaving rejected retry-only",`+
				`"severity":"error","action":"ask-user","caused_by_lineage_id":%q}]}`,
			lineageID,
			lineageID,
		))}, nil
	default:
		return nil, fmt.Errorf("unexpected routed prompt: %q", opts.Prompt)
	}
}

// This is the forensic §5 reproduction with the final assertion flipped to the
// FIXED behavior. An explicit, authoritative intent forbids retry-only and
// requires a guarded removal; the initial review raises an error/auto-fix race
// finding; the fixer "resolves" it by deleting the required behavior. Before
// the fix the intent-contradicting auto-fix completed silently. After the fix,
// the routed strong verifier's conformance obligation surfaces the
// contradiction as an ask-user finding, and one ask-user finding parks the run
// at the executor gate. The scripted routed agent models the initial reviewer,
// fixer, and independent verifier while the step exercises the real semantic
// invocation, repair-coordinator, persistence, and approval paths.
//
// The park is observable as the step reaching awaiting_approval with the run's
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

	cfg := &config.Config{Routing: config.DefaultRoutingConfig()}
	routedAgent := &intentConformanceAgent{intent: intent}

	stepCalls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			stepCalls++
			// The executor must propagate provenance so a step can tell an
			// authoritative intent from an inferred hint.
			if sctx.IntentSource != db.RunIntentSourceAgent {
				t.Errorf("IntentSource = %q, want %q", sctx.IntentSource, db.RunIntentSourceAgent)
			}
			if sctx.UserIntent != intent {
				t.Errorf("UserIntent = %q, want %q", sctx.UserIntent, intent)
			}
			result, err := sctx.InvokeAgent(types.PurposeInitialReview, agent.RunOpts{
				Prompt: "initial routed review",
				CWD:    sctx.WorkDir,
			})
			if err != nil {
				return nil, err
			}
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      string(result.Output),
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, routedAgent, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	// The run must park on the verifier-created ask-user finding rather than
	// silently completing after the routed fixer contradicted explicit intent.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if stepCalls != 1 || routedAgent.initialCalls != 1 || routedAgent.fixerCalls != 1 || routedAgent.verifyCalls != 1 {
		t.Errorf(
			"calls step=%d initial=%d fixer=%d verifier=%d, want one routed initial review, fixer, and verifier",
			stepCalls,
			routedAgent.initialCalls,
			routedAgent.fixerCalls,
			routedAgent.verifyCalls,
		)
	}
	stepRows, err := database.GetStepsByRun(run.ID)
	if err != nil || len(stepRows) != 1 || stepRows[0].FindingsJSON == nil {
		t.Fatalf("parked review step = %+v, err = %v", stepRows, err)
	}
	if findings := *stepRows[0].FindingsJSON; !strings.Contains(findings, "intent-required guarded removal") ||
		!strings.Contains(findings, `"action":"ask-user"`) {
		t.Fatalf("parked findings = %s, want explicit-intent contradiction requiring consent", findings)
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

	// Skip the unresolved ask-user finding so the executor goroutine exits.
	if err := exec.Respond(types.StepReview, types.ActionSkip, nil); err != nil {
		t.Fatalf("respond skip: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

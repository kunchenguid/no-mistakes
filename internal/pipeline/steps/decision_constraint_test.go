package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The fixer has restored teardown reclamation despite a recorded decision to
// remove it. The independent decision check reports the contradiction; the
// shared commit boundary must refuse it even if the fixer claimed success.
func TestCommitAgentFixes_RejectsRecordedFixDecisionReversal(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Session != nil {
			t.Fatal("decision check must not resume the fixer session")
		}
		if !strings.Contains(opts.Prompt, "remove-teardown-reclamation") {
			t.Fatal("decision check did not receive the recorded ruling")
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"recorded decision reversed","findings":[{"id":"decision-reversal","severity":"error","action":"ask-user","description":"review round 1 decision remove-teardown-reclamation is contradicted: teardown.txt restores build-output reclamation"}]}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
	if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim build outputs during teardown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := commitAgentFixes(sctx, types.StepTest, "restore teardown reclamation", "")
	if err == nil || !strings.Contains(err.Error(), "remove-teardown-reclamation") {
		t.Fatalf("recorded decision reversal was not rejected with a named finding: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("rejected fix advanced HEAD: %s", got)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatal("rejected fix advanced the recorded head")
	}
}

func recordReclamationDecision(t *testing.T, sctx *pipeline.StepContext, source string) {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"remove-teardown-reclamation","severity":"warning","action":"ask-user","file":"teardown.txt","description":"Remove teardown-side build-output reclamation"}]}`
	round, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["remove-teardown-reclamation"]`
	if source == db.RoundSelectionSourceUserDeclined {
		selected = "[]"
	}
	if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selected, source, &findings); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionCheck_OnlyPositiveHumanDecisionsAddAnInvocation(t *testing.T) {
	for _, source := range []string{"", db.RoundSelectionSourceAutoFix, db.RoundSelectionSourceUserDeclined, db.RoundSelectionSourceUser} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"decision preserved"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			if source != "" {
				recordReclamationDecision(t, sctx, source)
			}
			if err := assertRecordedFixDecisions(sctx); err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) != 0 {
				t.Fatal("unchanged tree invoked the checker")
			}
			if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("safe repair\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := commitAgentFixes(sctx, types.StepTest, "safe repair", ""); err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			if source == db.RoundSelectionSourceUser {
				wantCalls = 1
			}
			if len(ag.calls) != wantCalls {
				t.Fatalf("agent calls = %d, want %d", len(ag.calls), wantCalls)
			}
		})
	}
}

func TestTestStep_DecisionReversalParksWithDurableFinding(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		switch calls {
		case 1:
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"failing-test","severity":"error","action":"auto-fix","description":"test expects teardown reclamation"}],"summary":"test failed","tested":["focused check"],"testing_summary":"test failed","artifacts":[]}`)}, nil
		case 2:
			if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim build outputs during teardown\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"restore teardown reclamation"}`)}, nil
		default:
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"decision-reversal","severity":"error","action":"ask-user","description":"review round 1 remove-teardown-reclamation contradicted by teardown.txt"}],"summary":"recorded ruling reversed"}`)}, nil
		}
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.AutoFix.Test = 1
	recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
	var parkedEvent ipc.Event
	var executor *pipeline.Executor
	executor = pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusFixReview) {
			parkedEvent = event
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || run.AwaitingAgentSince == nil {
				t.Fatalf("decision conflict did not park the run: %+v, %v", run, err)
			}
			if err := executor.Respond(types.StepTest, types.ActionAbort, nil); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := executor.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil {
		t.Fatal("reversing repair silently passed")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.Status != types.RunFailed || run.HeadSHA != head {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	steps, err := sctx.DB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepTest && (step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "remove-teardown-reclamation")) {
			t.Fatalf("parked step lost its named decision finding: %+v", step)
		}
	}
	if calls != 3 || parkedEvent.Findings == nil || !strings.Contains(*parkedEvent.Findings, "remove-teardown-reclamation") {
		t.Fatalf("calls=%d, parked event=%+v", calls, parkedEvent)
	}
}

func TestDecisionCheck_UnusableOutputDoesNotCommit(t *testing.T) {
	for _, output := range []string{"", `{}`, `{"findings":null,"summary":"fine"}`, `{"findings":[{}],"summary":"fine"}`} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(output)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
			if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := commitAgentFixes(sctx, types.StepTest, "restore reclamation", ""); err == nil {
				t.Fatal("unusable decision check allowed the commit")
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("unverified repair advanced HEAD: %s", got)
			}
		})
	}
}

func TestReviewStep_RecordedFixDecisionSurvivesEmptyDiff(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Session != nil || !strings.Contains(opts.Prompt, "Complete same-run decision constraints") || !strings.Contains(opts.Prompt, "remove-teardown-reclamation") {
			t.Fatal("review did not independently check the complete decision history")
		}
		findings := cleanReviewFindings()
		findings.Items = []Finding{{ID: "decision-reversal", Severity: "error", Action: types.ActionAskUser, Description: "review round 1 remove-teardown-reclamation was undone"}}
		output, err := json.Marshal(findings)
		return &agent.Result{Output: output}, err
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
	gitCmd(t, dir, "checkout", "--detach", base)
	sctx.Run.HeadSHA = base
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, base); err != nil {
		t.Fatal(err)
	}
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !hasAskUserFindings(t, outcome.Findings) || len(ag.calls) != 1 {
		t.Fatalf("empty diff bypassed recorded decision conformance: outcome=%+v err=%v calls=%d", outcome, err, len(ag.calls))
	}
}

func TestTestStep_EvidenceFixCannotSilentlyReverseDecision(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"tests pass","tested":["focused test"],"testing_summary":"tests pass","artifacts":[]}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","action":"ask-user","description":"review round 1 remove-teardown-reclamation contradicted by teardown.txt"}],"summary":"recorded ruling reversed"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
	if outcome, err := (&TestStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "remove-teardown-reclamation") {
		t.Fatalf("evidence agent's zero-finding reversal passed: outcome=%+v, err=%v", outcome, err)
	}
}

func TestDecisionCheck_MissingSelectedFindingFailsClosed(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)
	sctx := f.testStepContext()
	sctx.StepResultID = f.reviewSR.ID
	selected := `["unavailable-ruling"]`
	if err := f.db.SetStepRoundUserDecision(mustLatestRoundID(t, sctx), &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := recordedFixConstraints(sctx); err == nil || !strings.Contains(err.Error(), "unavailable-ruling") {
		t.Fatalf("missing positive decision must be named and refused: %v", err)
	}
}

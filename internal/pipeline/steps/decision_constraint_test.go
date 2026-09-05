package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			logDecisionState(t, sctx, "test repair parked", event)
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
	logDecisionState(t, sctx, "operator aborted rejected test repair", nil)
}

// Emit real persisted state and Git state for evidence collection with go test
// -json. Agent verdicts are simulated by the fixtures, not live model judgments.
func logDecisionState(t *testing.T, sctx *pipeline.StepContext, stage string, observation any) {
	t.Helper()
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := sctx.DB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds := make(map[types.StepName][]*db.StepRound)
	for _, step := range steps {
		rounds[step.StepName], err = sctx.DB.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"stage": stage, "run": run, "steps": steps, "rounds": rounds,
		"git_head": gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"),
		"git_status": gitCmd(t, sctx.WorkDir, "status", "--porcelain"),
		"observation": observation,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("DECISION_EVIDENCE %s", encoded)
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

func TestRebaseStep_EmptyConflictResolutionStillReachesIndependentReview(t *testing.T) {
	t.Parallel()
	dir, base, _ := setupGitRepo(t)
	gitCmd(t, dir, "reset", "--hard", base)
	gitCmd(t, dir, "rm", "base.txt")
	gitCmd(t, dir, "commit", "-m", "remove superseded behavior")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("updated superseded behavior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "update upstream behavior")
	gitCmd(t, dir, "checkout", "feature")

	const instruction = "Preserve the branch's removal of base.txt"
	decisionID := ""
	reviews := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, instruction) {
			t.Fatal("agent did not receive the selected Rebase instructions")
		}
		if opts.Purpose != "review" {
			gitCmd(t, dir, "rebase", "--skip")
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolve conflict with upstream behavior"}`)}, nil
		}
		reviews++
		if opts.Session != nil {
			t.Fatal("post-Rebase Review must be independent")
		}
		if diff := gitCmd(t, dir, "diff", "origin/main", "HEAD"); diff != "" {
			t.Fatalf("expected the conflict resolution to erase the branch diff: %s", diff)
		}
		findings := cleanReviewFindings()
		findings.Items = []Finding{{ID: "decision-reversal", Severity: "error", Action: types.ActionAskUser, File: "base.txt", Description: "rebase round 1 " + decisionID + " contradicted by HEAD restoring base.txt"}}
		output, err := json.Marshal(findings)
		return &agent.Result{Output: output}, err
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = dir
	parkedReview := false
	var executor *pipeline.Executor
	executor = pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&RebaseStep{}, &ReviewStep{}}, func(event ipc.Event) {
		if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) || event.StepName == nil {
			return
		}
		switch *event.StepName {
		case types.StepRebase:
			if decisionID != "" || event.Findings == nil {
				t.Fatal("unexpected Rebase gate")
			}
			findings, err := types.ParseFindingsJSON(*event.Findings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("expected one removal conflict: %+v, %v", findings, err)
			}
			decisionID = findings.Items[0].ID
			if err := executor.RespondWithOverrides(types.StepRebase, types.ActionFix, []string{decisionID}, map[string]string{decisionID: instruction}, nil); err != nil {
				t.Fatal(err)
			}
		case types.StepReview:
			parkedReview = event.Findings != nil && strings.Contains(*event.Findings, decisionID) && hasAskUserFindings(t, *event.Findings)
			if err := executor.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
				t.Fatal(err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir)
	if err == nil || !parkedReview || reviews != 1 || len(ag.calls) != 2 {
		t.Fatalf("empty Rebase bypassed Review: err=%v parked=%t reviews=%d calls=%d", err, parkedReview, reviews, len(ag.calls))
	}
}

func TestRebaseStep_AcceptedEmptyResolutionSkipsPublicationAfterReview(t *testing.T) {
	for _, tc := range []struct {
		name, baseBranch, severity, action, wantError string
	}{
		{name: "accepted"},
		{name: "unusable_review", wantError: "missing risk assessment"},
		{name: "configured_base", baseBranch: "develop"},
		{name: "informational", severity: "info", action: types.ActionNoOp},
		{name: "configured_base_informational", baseBranch: "develop", severity: "info", action: types.ActionNoOp},
		{name: "blocking", severity: "warning", action: types.ActionNoOp, wantError: "aborted by user"},
		{name: "ask_user_info", severity: "info", action: types.ActionAskUser, wantError: "aborted by user"},
		{name: "missing_action_info", severity: "info", wantError: "aborted by user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, _ := setupGitRepo(t)
			gitCmd(t, dir, "reset", "--hard", base)
			gitCmd(t, dir, "rm", "base.txt")
			gitCmd(t, dir, "commit", "-m", "remove superseded behavior")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "main")
			if tc.baseBranch != "" {
				gitCmd(t, dir, "checkout", "-b", tc.baseBranch)
			}
			if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("updated upstream behavior\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "update upstream behavior")
			upstreamHead := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "feature")

			const instruction = "Accept upstream base.txt and drop the branch's removal"
			decisionID := ""
			reviews := 0
			parkedReview := false
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if !strings.Contains(opts.Prompt, instruction) {
					t.Fatal("agent did not receive the accepted upstream decision")
				}
				if opts.Purpose != "review" {
					gitCmd(t, dir, "rebase", "--skip")
					return &agent.Result{Output: json.RawMessage(`{"summary":"accept upstream behavior"}`)}, nil
				}
				reviews++
				if opts.Session != nil || gitCmd(t, dir, "rev-parse", "HEAD") != upstreamHead {
					t.Fatal("expected independent Review of HEAD equal to the integration base")
				}
				if tc.name == "unusable_review" {
					return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
				}
				findings := cleanReviewFindings()
				if tc.severity != "" {
					findings.Items = []Finding{{ID: "rebase-decision-note", Severity: tc.severity, Action: tc.action, Description: "Review note for rebase round 1 " + decisionID}}
				}
				output, err := json.Marshal(findings)
				return &agent.Result{Output: output}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Repo.UpstreamURL = dir
			sctx.Config.PR.BaseBranch = tc.baseBranch
			var executor *pipeline.Executor
			executor = pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag,
				[]pipeline.Step{&RebaseStep{}, &ReviewStep{}, &PushStep{}, &PRStep{}, &CIStep{}}, func(event ipc.Event) {
					if event.StepName == nil {
						return
					}
					if event.Type == ipc.EventStepStarted && *event.StepName != types.StepRebase && *event.StepName != types.StepReview {
						t.Fatalf("empty Rebase reached publication step %s", *event.StepName)
					}
					if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) {
						return
					}
					if *event.StepName == types.StepReview && tc.wantError == "aborted by user" {
						if event.Findings == nil || !strings.Contains(*event.Findings, decisionID) {
							t.Fatal("Review gate lost the named decision finding")
						}
						parkedReview = true
						if err := executor.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
							t.Fatal(err)
						}
						return
					}
					if *event.StepName != types.StepRebase || decisionID != "" || event.Findings == nil {
						t.Fatal("unexpected approval gate")
					}
					findings, err := types.ParseFindingsJSON(*event.Findings)
					if err != nil || len(findings.Items) != 1 {
						t.Fatalf("expected one removal conflict: %+v, %v", findings, err)
					}
					decisionID = findings.Items[0].ID
					if err := executor.RespondWithOverrides(types.StepRebase, types.ActionFix, []string{decisionID}, map[string]string{decisionID: instruction}, nil); err != nil {
						t.Fatal(err)
					}
				})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("unexpected pipeline result: %v", err)
			}
			if parkedReview != (tc.wantError == "aborted by user") {
				t.Fatalf("Review approval gate reached = %t, expected error = %q", parkedReview, tc.wantError)
			}
			if decisionID == "" || reviews != 1 || len(ag.calls) != 2 {
				t.Fatalf("missing independent Review after accepted Rebase: decision=%q reviews=%d calls=%d", decisionID, reviews, len(ag.calls))
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantError == "" {
				if run.Status != types.RunCompleted || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != upstreamHead {
					t.Fatalf("accepted empty Review did not complete with its reviewed head: %+v", run)
				}
			} else if run.Status != types.RunFailed || run.ReviewApprovedHeadSHA != nil {
				t.Fatalf("unusable Review certified the empty state: %+v", run)
			}
			if run.LastPushedSHA != nil || run.PRURL != nil {
				t.Fatalf("empty Rebase changed publication state: %+v", run)
			}
			steps, err := sctx.DB.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				if tc.wantError == "" && step.StepName != types.StepRebase && step.StepName != types.StepReview && step.Status != types.StepStatusSkipped {
					t.Fatalf("publication step %s was not skipped: %s", step.StepName, step.Status)
				}
				if step.StepName == types.StepReview && tc.severity != "" {
					if step.FindingsJSON == nil {
						t.Fatal("Review lost its findings")
					}
					findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
					if err != nil || len(findings.Items) != 1 || findings.Items[0].ID != "rebase-decision-note" {
						t.Fatalf("Review did not preserve the finding: %+v, %v", findings, err)
					}
				}
			}
			logDecisionState(t, sctx, "empty rebase after independent review", map[string]any{
				"integration_base": effectivePRBaseBranch(sctx), "integration_head": upstreamHead,
				"review_invocations": reviews, "review_parked": parkedReview,
			})
		})
	}
}

func TestReviewStep_IgnoredChangesDoNotSkipPublication(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		output, err := json.Marshal(cleanReviewFindings())
		return &agent.Result{Output: output}, err
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.IgnorePatterns = []string{"*.txt"}
	recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil || outcome.NeedsApproval || outcome.SkipRemaining || len(ag.calls) != 1 {
		t.Fatalf("ignored changes were treated as an empty branch: outcome=%+v err=%v calls=%d", outcome, err, len(ag.calls))
	}
}

func TestCIStep_RejectedRebasePreservesGateAndFixReentry(t *testing.T) {
	for _, tc := range []struct {
		name, checks     string
		noChange, closed bool
	}{
		{"failing", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`, false, false},
		{"green", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, false, false},
		{"pending", `[{"name":"test","state":"PENDING","bucket":"pending"}]`, false, false},
		{"no_change_green", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, true, false},
		{"closed", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			recordReclamationDecision(t, f.sctx, db.RoundSelectionSourceUser)
			gitCmd(t, f.dir, "checkout", "main")
			for _, file := range []string{"feature.txt", "teardown.txt"} {
				if err := os.WriteFile(filepath.Join(f.dir, file), []byte("reclaim build outputs during teardown\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "restore upstream teardown behavior")
			gitCmd(t, f.dir, "push", "origin", "main")
			gitCmd(t, f.dir, "checkout", "feature")
			sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			f.sctx.StepResultID = sr.ID
			calls := 0
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				switch calls {
				case 1:
					if _, err := stepGitRun(f.sctx, "rebase", "origin/main"); err == nil {
						t.Fatal("expected a real rebase conflict")
					}
					gitCmd(t, f.dir, "rebase", "--skip")
					return &agent.Result{Output: json.RawMessage(`{"summary":"resolve conflict","code_change_needed":true}`)}, nil
				case 2:
					return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"decision-reversal","severity":"error","action":"ask-user","file":"teardown.txt","description":"review round 1 remove-teardown-reclamation contradicted by HEAD restoring teardown.txt"}],"summary":"recorded ruling reversed"}`)}, nil
				case 3:
					if !strings.Contains(opts.Prompt, "Remove the rejected teardown restoration") {
						t.Fatal("Fix re-entry lost the new per-finding instructions")
					}
					if tc.noChange {
						return &agent.Result{Output: json.RawMessage(`{"summary":"no change proposed","code_change_needed":false}`)}, nil
					}
					if err := os.Remove(filepath.Join(f.dir, "teardown.txt")); err != nil {
						t.Fatal(err)
					}
					return &agent.Result{Output: json.RawMessage(`{"summary":"preserve teardown removal","code_change_needed":true}`)}, nil
				case 4:
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"recorded ruling preserved"}`)}, nil
				default:
					t.Fatalf("unexpected agent call %d", calls)
					return nil, nil
				}
			}}
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				return context.DeadlineExceeded
			}}
			_, err = step.Execute(f.sctx)
			var conflict *pipeline.DecisionConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("rejected repair did not surface a decision gate: %v", err)
			}
			rejectedHead := f.localHead(t)
			if rejectedHead == f.headSHA {
				t.Fatal("rejection did not preserve the rewritten head")
			}
			raw, err := types.MarshalFindingsJSON(conflict.Findings)
			if err != nil {
				t.Fatal(err)
			}
			round, err := f.sctx.DB.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.sctx.DB.ParkStepForApproval(f.sctx.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, &raw); err != nil {
				t.Fatal(err)
			}
			if resolved, err := step.ReconcileApprovalGate(f.sctx); err != nil || resolved {
				t.Fatalf("rejected repair cannot remain parked: resolved=%t err=%v", resolved, err)
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.AwaitingAgentSince == nil || run.HeadSHA != rejectedHead || f.sctx.Run.HeadSHA != rejectedHead || run.ReviewApprovedHeadSHA != nil || f.sctx.Run.ReviewApprovedHeadSHA != nil {
				t.Fatalf("rejected repair lost its parked, unapproved state: %+v", run)
			}
			if run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA || f.remoteHead(t) != f.headSHA {
				t.Fatal("rejected repair changed publication state")
			}
			if err := assertReviewApprovedPushHead(f.sctx, rejectedHead); err == nil {
				t.Fatal("rejected repair gained publication authority")
			}
			logDecisionState(t, f.sctx, "CI rejection reconciled at approval gate", map[string]any{
				"remote_head": f.remoteHead(t), "publication_error": assertReviewApprovedPushHead(f.sctx, rejectedHead).Error(),
			})
			conflict.Findings.Items[0].UserInstructions = "Remove the rejected teardown restoration"
			selected := `["decision-reversal"]`
			userFindings, err := types.MarshalFindingsJSON(conflict.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.sctx.DB.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, &userFindings); err != nil {
				t.Fatal(err)
			}
			prState := "OPEN"
			if tc.closed {
				prState = "CLOSED"
			}
			f.sctx.Env = fakeCIGH(t, prState, tc.checks)
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = userFindings
			outcome, err := step.Execute(f.sctx)
			wantCalls := 4
			wantRestart := types.StepReview
			if tc.closed {
				wantCalls = 2
				wantRestart = ""
			}
			if tc.noChange {
				wantCalls = 3
			}
			if err != nil || outcome == nil || outcome.RestartFrom != wantRestart || calls != wantCalls {
				t.Fatalf("Fix re-entry failed to return the corrected repair to Review: outcome=%+v err=%v calls=%d", outcome, err, calls)
			}
			run, err = f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run.CIReadyAt != nil {
				t.Fatalf("unpublished decision repair became ready: run=%+v err=%v", run, err)
			}
			if f.remoteHead(t) != f.headSHA || assertReviewApprovedPushHead(f.sctx, f.localHead(t)) == nil {
				t.Fatal("corrected repair published before independent Review")
			}
			logDecisionState(t, f.sctx, "CI selected Fix returned", map[string]any{
				"remote_head": f.remoteHead(t), "outcome": outcome, "agent_invocations": calls,
				"checks": json.RawMessage(tc.checks), "pr_state": prState,
			})
		})
	}
}

func TestReviewStep_EmittedDecisionHistoryBindsOnlySameRunChoices(t *testing.T) {
	for _, scope := range []string{"current_step", "other_step", "earlier_run"} {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if !strings.Contains(opts.Prompt, "remove-teardown-reclamation") {
					t.Fatal("emitted Review prompt dropped the recorded choice")
				}
				binding := strings.Contains(opts.Prompt, "are binding acceptance criteria")
				if binding != (scope != "earlier_run") {
					t.Fatalf("emitted Review prompt binding=%t for %s", binding, scope)
				}
				output, err := json.Marshal(cleanReviewFindings())
				return &agent.Result{Output: output}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
			if scope == "earlier_run" {
				run, err := sctx.DB.InsertRun(sctx.Repo.ID, sctx.Run.Branch, head, base)
				if err != nil {
					t.Fatal(err)
				}
				sctx.Run = run
				pipeline.BindBranchDecisions(sctx)
			} else if scope == "current_step" {
				steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
				if err != nil || len(steps) != 1 {
					t.Fatalf("load recorded step: %+v, %v", steps, err)
				}
				sctx.StepResultID = steps[0].ID
			}
			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err != nil || outcome == nil || outcome.NeedsApproval || len(ag.calls) != 1 {
				t.Fatalf("Review outcome=%+v err=%v calls=%d", outcome, err, len(ag.calls))
			}
		})
	}
}

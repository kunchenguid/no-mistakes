package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// After an operator approves a parked decision conflict, the next commit
// boundary must not re-check a tree the human already ruled on; only a new
// change after that ruling buys another check.
func TestDecisionCheck_ApprovedRulingIsNotRecheckedOnUnchangedTree(t *testing.T) {
	for _, tc := range []struct {
		name           string
		trackedIgnored bool
		documentEdits  string
		indexEdit      string
		wantChecks     int
	}{
		{name: "unchanged_tree", wantChecks: 1},
		{name: "new_change_after_ruling", documentEdits: "README.md", wantChecks: 2},
		{name: "new_change_in_tracked_ignored_file", trackedIgnored: true, documentEdits: "ignored.txt", wantChecks: 2},
		{name: "staged_ignored_removal", trackedIgnored: true, indexEdit: "remove", wantChecks: 2},
		{name: "staged_ignored_addition", trackedIgnored: true, indexEdit: "add", wantChecks: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			if tc.trackedIgnored {
				for name, content := range map[string]string{".gitignore": "ignored*.txt\n", "ignored.txt": "tracked despite the ignore rule\n"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				gitCmd(t, dir, "add", "-f", ".gitignore", "ignored.txt")
				gitCmd(t, dir, "commit", "-m", "track a file the ignore rule matches")
				head = gitCmd(t, dir, "rev-parse", "HEAD")
			}
			testCalls, checks := 0, 0
			ag := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				switch opts.Purpose {
				case "decision-conformance":
					checks++
					if checks == 1 {
						return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"decision-reversal","severity":"error","action":"ask-user","description":"review round 1 remove-teardown-reclamation contradicted by teardown.txt"}],"summary":"recorded ruling reversed"}`)}, nil
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ruling already accepted"}`)}, nil
				case "housekeeping":
					switch tc.indexEdit {
					case "remove":
						gitCmd(t, dir, "rm", "--cached", "ignored.txt")
					case "add":
						if err := os.WriteFile(filepath.Join(dir, "ignored-new.txt"), []byte("new staged content\n"), 0o644); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, dir, "add", "-f", "ignored-new.txt")
					}
					if tc.indexEdit != "" {
						indexPath := filepath.Join(dir, ".git", "index")
						before, err := os.ReadFile(indexPath)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := worktreeTreeSHA(sctx); err != nil {
							t.Fatal(err)
						}
						after, err := os.ReadFile(indexPath)
						if err != nil || string(before) != string(after) {
							t.Fatalf("snapshot changed the real index: %v", err)
						}
					}
					if tc.documentEdits != "" {
						if err := os.WriteFile(filepath.Join(dir, tc.documentEdits), []byte("edited after the ruling\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
				}
				testCalls++
				if testCalls == 1 {
					return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"failing-test","severity":"error","action":"auto-fix","description":"test expects teardown reclamation"}],"summary":"test failed","tested":["focused check"],"testing_summary":"test failed","artifacts":[]}`)}, nil
				}
				if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim build outputs during teardown\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"restore teardown reclamation"}`)}, nil
			}
			sctx.Config.AutoFix.Test = 1
			recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
			approved := false
			var executor *pipeline.Executor
			executor = pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}, &DocumentStep{}}, func(event ipc.Event) {
				if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusFixReview) {
					return
				}
				if approved || event.StepName == nil || *event.StepName != types.StepTest {
					t.Fatalf("unexpected gate: %+v", event)
				}
				approved = true
				logDecisionState(t, sctx, "test repair parked", event)
				if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
					t.Fatal(err)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir); err != nil || !approved {
				t.Fatalf("run did not complete after the operator ruling: err=%v approved=%t", err, approved)
			}
			logDecisionState(t, sctx, "run completed after operator ruling", map[string]int{"decision_checks": checks})
			if checks != tc.wantChecks {
				t.Fatalf("decision checks = %d, want %d", checks, tc.wantChecks)
			}
		})
	}
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
		"git_head":    gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"),
		"git_status":  gitCmd(t, sctx.WorkDir, "status", "--porcelain"),
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

func TestTestStep_ApprovedEvidenceConflictPreservesTestingSection(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
	p := paths.WithRoot(t.TempDir())
	evidenceDir := p.RunEvidenceDir("", sctx.Run.ID)
	artifactPath := filepath.Join(evidenceDir, "teardown.log")
	const summary = "Exercised teardown and captured its reclamation output"
	const transcript = "teardown: reclaimed build outputs"
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "decision-conformance" {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"decision-reversal","severity":"error","action":"ask-user","description":"review round 1 remove-teardown-reclamation contradicted by teardown.txt"}],"summary":"recorded ruling reversed"}`)}, nil
		}
		if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(artifactPath, []byte(transcript), 0o644); err != nil {
			t.Fatal(err)
		}
		output, err := json.Marshal(Findings{
			Items:   []Finding{{ID: "evidence-note", Severity: "info", Action: types.ActionNoOp, Description: "Captured teardown output"}},
			Summary: "tests pass", Tested: []string{"teardown --dry-run"}, TestingSummary: summary,
			Artifacts: []types.TestArtifact{{Kind: "log", Label: "Teardown transcript", Path: artifactPath}},
		})
		return &agent.Result{Output: output}, err
	}
	assertEvidence := func(raw string) {
		t.Helper()
		findings, err := types.ParseFindingsJSON(raw)
		if err != nil || len(findings.Items) != 2 || findings.Items[0].ID != "evidence-note" || findings.Items[1].ID != "decision-reversal" || findings.Items[1].Action != types.ActionAskUser {
			t.Errorf("evidence and conflict findings not preserved: %+v, %v", findings, err)
		}
		if len(findings.Tested) != 1 || findings.Tested[0] != "teardown --dry-run" || findings.TestingSummary != summary || len(findings.Artifacts) != 1 || findings.Artifacts[0].Path != artifactPath {
			t.Errorf("decision conflict lost analyzer evidence: %+v", findings)
		}
	}
	parked := false
	var executor *pipeline.Executor
	executor = pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, []pipeline.Step{&TestStep{}}, func(event ipc.Event) {
		if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) {
			return
		}
		parked = true
		if event.Findings == nil {
			t.Fatal("parked without findings")
		}
		assertEvidence(*event.Findings)
		run, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil || run.AwaitingAgentSince == nil {
			t.Fatalf("conflict not durably parked: %+v, %v", run, err)
		}
		results, err := sctx.DB.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range results {
			if result.StepName != types.StepTest {
				continue
			}
			if result.FindingsJSON == nil {
				t.Fatal("Test has no persisted evidence")
			}
			assertEvidence(*result.FindingsJSON)
			rounds, err := sctx.DB.GetRoundsByStep(result.ID)
			if err != nil || len(rounds) != 1 || rounds[0].FindingsJSON == nil {
				t.Fatalf("Test round not persisted: %+v, %v", rounds, err)
			}
			assertEvidence(*rounds[0].FindingsJSON)
		}
		logDecisionState(t, sctx, "Test evidence conflict parked with analyzer evidence", event)
		if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
			t.Fatal(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir); err != nil || !parked {
		t.Fatalf("conflict approval did not complete: parked=%t, %v", parked, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.Status != types.RunCompleted || run.HeadSHA != head || gitCmd(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("evidence approval changed completion or commit semantics: %+v, %v", run, err)
	}
	if err := assertRecordedFixDecisions(sctx); err != nil || len(ag.calls) != 2 {
		t.Fatalf("approved evidence tree lost its checked-tree ruling: calls=%d, %v", len(ag.calls), err)
	}
	results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds := make(map[string][]*db.StepRound)
	for _, result := range results {
		rounds[result.ID], err = sctx.DB.GetRoundsByStep(result.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	testingSection := BuildTestingSummaryForPR(results, rounds, sctx.Repo.UpstreamURL, head, dir, evidenceDir, nil)
	for _, want := range []string{summary, "Teardown transcript", transcript} {
		if !strings.Contains(testingSection, want) {
			t.Errorf("Testing section lost %q:\n%s", want, testingSection)
		}
	}
	t.Logf("APPROVED_EVIDENCE_TESTING_SECTION\n%s", testingSection)
	tree, err := worktreeTreeSHA(sctx)
	if err != nil {
		t.Fatal(err)
	}
	ruled, err := sctx.DB.HasDeclinedDecisionCheck(sctx.Run.ID, tree, "")
	if err != nil || !ruled {
		t.Fatalf("approved evidence tree has no persisted ruling: tree=%s ruled=%t err=%v", tree, ruled, err)
	}
	logDecisionState(t, sctx, "Test evidence retained after operator approval", map[string]any{
		"checked_tree_sha": tree, "persisted_ruling": ruled,
	})
}

func TestDecisionCheck_NewerPositiveDecisionSupersedesTreeRuling(t *testing.T) {
	for _, tc := range []struct {
		name, selection string
		wantConflict    bool
	}{
		{name: "matched", selection: `["remove-again"]`, wantConflict: true},
		{name: "mixed", selection: `["remove-again","typo"]`, wantConflict: true},
		{name: "unmatched", selection: `["typo"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"decision-reversal","severity":"error","action":"ask-user","description":"teardown.txt restores reclamation against the latest positive decision"}],"summary":"recorded decision reversed"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
			writeTree := func(content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeTree("reclaim\n")
			var conflict *pipeline.DecisionConflictError
			if err := assertRecordedFixDecisions(sctx); !errors.As(err, &conflict) {
				t.Fatalf("initial reversal did not conflict: %v", err)
			}
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := types.MarshalFindingsJSON(conflict.Findings)
			if err != nil {
				t.Fatal(err)
			}
			ruling, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepRoundCheckedTree(ruling.ID, conflict.CheckedTreeSHA); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepRoundDeclined(ruling.ID); err != nil {
				t.Fatal(err)
			}
			if err := assertRecordedFixDecisions(sctx); err != nil || len(ag.calls) != 1 {
				t.Fatalf("unchanged tree exemption was lost: %v, calls=%d", err, len(ag.calls))
			}
			ci, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"remove-again","severity":"warning","action":"ask-user","description":"Remove reclamation again","user_instructions":"Remove teardown-side build-output reclamation"}]}`
			decision, err := sctx.DB.InsertStepRound(ci.ID, 1, "initial", &findings, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepRoundUserDecision(decision.ID, &tc.selection, db.RoundSelectionSourceUser, &findings); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "record approved tree")
			writeTree("keep outputs\n")
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "implement latest decision")
			sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, sctx.Run.HeadSHA); err != nil {
				t.Fatal(err)
			}
			writeTree("reclaim\n")
			err = commitAgentFixes(sctx, types.StepCI, "restore old tree", "")
			if tc.wantConflict {
				if !errors.As(err, &conflict) || len(ag.calls) != 2 {
					t.Fatalf("old ruling bypassed the newer positive decision: %v, checks=%d", err, len(ag.calls))
				}
				if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != sctx.Run.HeadSHA {
					t.Fatalf("rejected repair changed HEAD: %s", got)
				}
				raw, err := types.MarshalFindingsJSON(conflict.Findings)
				if err != nil {
					t.Fatal(err)
				}
				currentRuling, err := sctx.DB.InsertStepRound(ci.ID, 2, "auto_fix", &raw, nil, 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := sctx.DB.SetStepRoundCheckedTree(currentRuling.ID, conflict.CheckedTreeSHA); err != nil {
					t.Fatal(err)
				}
				if err := sctx.DB.SetStepRoundDeclined(currentRuling.ID); err != nil {
					t.Fatal(err)
				}
				if err := commitAgentFixes(sctx, types.StepCI, "publish newly approved tree", ""); err != nil || len(ag.calls) != 2 {
					t.Fatalf("current unchanged-tree ruling was ignored: %v, checks=%d", err, len(ag.calls))
				}
			} else if err != nil || len(ag.calls) != 1 {
				t.Fatalf("unmatched selection invalidated a legitimate exemption: %v, checks=%d", err, len(ag.calls))
			}
		})
	}
}

func TestDecisionCheck_UnreadableSelectionFailsClosed(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	f.declineReviewRound(t, declinedDedupFindings)
	sctx := f.testStepContext()
	sctx.StepResultID = f.reviewSR.ID
	selected := `["journal-version-deduplication"`
	if err := f.db.SetStepRoundUserDecision(mustLatestRoundID(t, sctx), &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordedFixConstraints(sctx); err == nil || !strings.Contains(err.Error(), "read decision") {
		t.Fatalf("unreadable selection must be refused: %v", err)
	}
}

// A fix response may name an ID from an earlier round or carry a typo. Such a
// selection is no positive decision: Review and every later commit boundary
// treat it as a logged no-op, never as a run failure, and a mixed selection
// binds only the IDs that matched a finding so the log and the binding text agree.
func TestDecisionCheck_UnmatchedSelectedFindingIsNoOp(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose != "review" {
			t.Fatalf("unmatched selection paid a %q pass", opts.Purpose)
		}
		output, err := json.Marshal(cleanReviewFindings())
		return &agent.Result{Output: output}, err
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","action":"ask-user","file":"teardown.txt","description":"Remove teardown-side build-output reclamation"}]}`
	round, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["review-9"]`
	if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil || outcome.NeedsApproval || len(ag.calls) != 1 {
		t.Fatalf("unmatched selection failed Review: outcome=%+v err=%v calls=%d", outcome, err, len(ag.calls))
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("safe repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepTest, "safe repair", ""); err != nil {
		t.Fatalf("unmatched selection failed the commit boundary: %v", err)
	}
	if len(ag.calls) != 1 || gitCmd(t, dir, "rev-parse", "HEAD") == head {
		t.Fatalf("unmatched selection changed the safe repair: calls=%d", len(ag.calls))
	}
	named := false
	for _, line := range logs {
		named = named || strings.Contains(line, `"review-9"`)
	}
	if !named {
		t.Fatalf("no log line named the unmatched selection: %q", logs)
	}
	selected = `["review-1","review-9"]`
	if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	decisions, _, err := recordedFixConstraints(sctx)
	if err != nil {
		t.Fatalf("mixed selection failed instead of binding the matched finding: %v", err)
	}
	if !strings.Contains(decisions, `user chose to fix: {"id":"review-1"`) || strings.Contains(decisions, "declined") || strings.Contains(decisions, "review-9") {
		t.Fatalf("mixed selection must bind only the matched finding as a positive decision: %q", decisions)
	}
}

// The binding channel cannot drop old lines the way the advisory prompt
// sections do, so a run recording many instructed decisions must still bind
// rather than fail at the boundary that consumes the complete history.
func TestDecisionCheck_LargeSameRunHistoryStillBinds(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"decisions preserved"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	instructions := strings.Repeat("Keep the teardown removal out of every later repair, including Rebase and CI fixes. ", 20)
	const rounds = 60
	for i := 1; i <= rounds; i++ {
		id := fmt.Sprintf("decision-%d", i)
		findings := fmt.Sprintf(`{"findings":[{"id":%q,"severity":"warning","action":"ask-user","file":"teardown.txt","description":"Remove teardown-side build-output reclamation %d"}]}`, id, i)
		round, err := sctx.DB.InsertStepRound(sr.ID, i, "initial", &findings, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		userFindings := fmt.Sprintf(`{"findings":[{"id":%q,"severity":"warning","action":"ask-user","file":"teardown.txt","description":"Remove teardown-side build-output reclamation %d","user_instructions":%q}]}`, id, i, instructions)
		selected := fmt.Sprintf(`[%q]`, id)
		if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, &userFindings); err != nil {
			t.Fatal(err)
		}
	}
	decisions, _, err := recordedFixConstraints(sctx)
	if err != nil {
		t.Fatalf("large same-run history failed instead of binding: %v", err)
	}
	if len(decisions) <= maxDecisionSectionBytes {
		t.Fatalf("history of %d bytes does not exceed the advisory prompt cap of %d", len(decisions), maxDecisionSectionBytes)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("safe repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepTest, "safe repair", ""); err != nil {
		t.Fatalf("large same-run history failed the commit boundary: %v", err)
	}
	if len(ag.calls) != 1 || ag.calls[0].Purpose != "decision-conformance" {
		t.Fatalf("expected one decision check, got %d calls", len(ag.calls))
	}
	for _, id := range []string{`"decision-1"`, fmt.Sprintf(`"decision-%d"`, rounds)} {
		if !strings.Contains(ag.calls[0].Prompt, id) {
			t.Fatalf("decision check prompt dropped %s", id)
		}
	}
	if gitCmd(t, dir, "rev-parse", "HEAD") == head {
		t.Fatal("checked repair was not committed")
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
		approve, recovered                            bool
		skippedTest                                   bool
	}{
		{name: "accepted"},
		{name: "approve_blocking", baseBranch: "develop", severity: "warning", action: types.ActionNoOp, approve: true},
		{name: "approve_ask_user", baseBranch: "develop", severity: "info", action: types.ActionAskUser, approve: true},
		{name: "recovered_approve_blocking", baseBranch: "develop", severity: "warning", action: types.ActionNoOp, approve: true, recovered: true},
		{name: "recovered_approve_ask_user", baseBranch: "develop", severity: "info", action: types.ActionAskUser, approve: true, recovered: true},
		{name: "recovered_approve_preserves_skipped_test", baseBranch: "develop", severity: "warning", action: types.ActionNoOp, approve: true, recovered: true, skippedTest: true},
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
			stopped := false
			stop := errors.New("daemon stopped at durable Review gate")
			p := paths.WithRoot(t.TempDir())
			plan := []pipeline.Step{&RebaseStep{}, &ReviewStep{}, &TestStep{}, &PushStep{}, &PRStep{}, &CIStep{}}
			emit := func(event ipc.Event) {
				if event.StepName == nil {
					return
				}
				if event.Type == ipc.EventStepStarted && *event.StepName != types.StepRebase && *event.StepName != types.StepReview {
					t.Fatalf("empty Rebase reached publication step %s", *event.StepName)
				}
				if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) {
					return
				}
				if *event.StepName == types.StepReview && (tc.approve || tc.wantError == "aborted by user") {
					if event.Findings == nil || !strings.Contains(*event.Findings, decisionID) {
						t.Fatal("Review gate lost the named decision finding")
					}
					parkedReview = true
					if tc.recovered && !stopped {
						stopped = true
						panic(stop)
					}
					action := types.ActionAbort
					if tc.approve {
						action = types.ActionApprove
					}
					if err := executor.Respond(types.StepReview, action, nil); err != nil {
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
			}
			executor = pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, plan, emit)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var err error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil && recovered != stop {
						panic(recovered)
					}
				}()
				err = executor.Execute(ctx, sctx.Run, sctx.Repo, dir)
			}()
			if tc.recovered {
				if !stopped {
					t.Fatal("Review never persisted a recoverable gate")
				}
				if tc.skippedTest {
					results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, result := range results {
						if result.StepName == types.StepTest {
							if err := sctx.DB.CompleteStepWithStatus(result.ID, types.StepStatusSkipped, 0, 42, "prior-test.log"); err != nil {
								t.Fatal(err)
							}
						}
					}
					if err := sctx.DB.ResetStepsFrom(sctx.Run.ID, types.StepTest.Order()); err != nil {
						t.Fatal(err)
					}
				}
				run, readErr := sctx.DB.GetRun(sctx.Run.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				executor = pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, plan, emit)
				err = executor.Resume(ctx, run, sctx.Repo, dir)
			}
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("unexpected pipeline result: %v", err)
			}
			if parkedReview != (tc.approve || tc.wantError == "aborted by user") {
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
				if tc.skippedTest && step.StepName == types.StepTest && (step.DurationMS == nil || *step.DurationMS != 42 || step.LogPath == nil || *step.LogPath != "prior-test.log") {
					t.Fatalf("recovery overwrote the skipped Test record: %+v", step)
				}
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

func TestReviewStep_ForwardFixCommitDoesNotSkipPublication(t *testing.T) {
	t.Parallel()
	dir, base, _ := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", base)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, base, config.Commands{})
	recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"remove-teardown-reclamation","severity":"warning","action":"auto-fix","description":"Record the teardown removal"}]}`
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "review-fix" {
			if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("teardown without reclamation\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "preserve teardown removal")
			return &agent.Result{Output: json.RawMessage(`{"summary":"preserve teardown removal"}`)}, nil
		}
		if opts.Purpose != "review" || opts.Session != nil {
			t.Fatalf("expected an independent rereview, got %+v", opts)
		}
		output, err := json.Marshal(cleanReviewFindings())
		return &agent.Result{Output: output}, err
	}
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil || outcome == nil {
		t.Fatalf("review failed: outcome=%+v err=%v", outcome, err)
	}
	live := gitCmd(t, dir, "rev-parse", "HEAD")
	if live == base || gitCmd(t, dir, "status", "--porcelain") != "" || len(ag.calls) != 2 {
		t.Fatal("expected one committed repair followed by an independent rereview")
	}
	if outcome.SkipRemaining || outcome.NeedsApproval || outcome.ReviewApprovedHeadSHA != live {
		t.Errorf("forward repair was treated as empty or approved at the stale head: %+v", outcome)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.HeadSHA != live || sctx.Run.HeadSHA != live || gitCmd(t, dir, "rev-parse", "refs/heads/feature") != live {
		t.Errorf("forward repair head was not reconciled: run=%+v err=%v", run, err)
	}
	uncertified, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
	if err != nil || uncertified == nil || uncertified.FromSHA != base || uncertified.ToSHA != live {
		t.Errorf("agent-created review commit lost its provenance: %+v, %v", uncertified, err)
	}
}

func TestTestStep_CollidingEvidenceConflictIDsRemainIndependentlySelectable(t *testing.T) {
	for _, selectedIndex := range []int{0, 1} {
		t.Run(fmt.Sprint(selectedIndex), func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			recordReclamationDecision(t, sctx, db.RoundSelectionSourceUser)
			descriptions := []string{"Capture the missing teardown trace", "Preserve the recorded teardown removal"}
			const instruction = "Repair only this selected finding"
			fixed, parked := false, false
			selectedID := ""
			ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				switch opts.Purpose {
				case "decision-conformance":
					output, err := json.Marshal(Findings{Items: []Finding{{ID: "test-1", Severity: "error", Action: types.ActionAskUser, Description: descriptions[1]}}})
					return &agent.Result{Output: output}, err
				case "test-fix":
					_, raw, ok := strings.Cut(opts.Prompt, "Previous test findings to address:\n")
					var selected Findings
					if !ok {
						t.Fatal("fixer received no selected findings")
					}
					if err := json.NewDecoder(strings.NewReader(raw)).Decode(&selected); err != nil {
						t.Fatal(err)
					}
					if len(selected.Items) != 1 || selected.Items[0].ID != selectedID || selected.Items[0].Description != descriptions[selectedIndex] || selected.Items[0].UserInstructions != instruction {
						t.Errorf("fixer received findings or instructions beyond the selection: %+v", selected)
					}
					fixed = true
					if err := os.Remove(filepath.Join(dir, "teardown.txt")); err != nil {
						t.Fatal(err)
					}
					return &agent.Result{Output: json.RawMessage(`{"summary":"repair selected finding"}`)}, nil
				}
				findings := Findings{Items: []Finding{}, Tested: []string{"teardown check"}, TestingSummary: "Captured teardown output", Artifacts: []types.TestArtifact{{Kind: "log", Label: "Teardown output", Content: "teardown checked"}}}
				if !fixed {
					if err := os.WriteFile(filepath.Join(dir, "teardown.txt"), []byte("reclaim\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					findings.Items = []Finding{{ID: "test-1", Severity: "warning", Action: types.ActionAskUser, Description: descriptions[0]}}
				}
				output, err := json.Marshal(findings)
				return &agent.Result{Output: output}, err
			}
			var executor *pipeline.Executor
			executor = pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, func(event ipc.Event) {
				if event.Type != ipc.EventStepCompleted || event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) {
					return
				}
				if parked || event.Findings == nil {
					t.Fatal("unexpected Test approval gate")
				}
				parked = true
				findings, err := types.ParseFindingsJSON(*event.Findings)
				if err != nil || len(findings.Items) != 2 {
					t.Fatalf("combined findings missing: %+v, %v", findings, err)
				}
				if findings.Items[0].ID == "" || findings.Items[1].ID == "" || findings.Items[0].ID == findings.Items[1].ID {
					t.Errorf("combined findings have colliding IDs: %+v", findings.Items)
				}
				selectedID = findings.Items[selectedIndex].ID
				if err := executor.RespondWithOverrides(types.StepTest, types.ActionFix, []string{selectedID}, map[string]string{selectedID: instruction}, nil); err != nil {
					t.Fatal(err)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir); err != nil || !parked || !fixed {
				t.Fatalf("selected fix did not complete: parked=%t fixed=%t err=%v", parked, fixed, err)
			}
			results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range results {
				if result.StepName != types.StepTest {
					continue
				}
				rounds, err := sctx.DB.GetRoundsByStep(result.ID)
				if err != nil || len(rounds) != 2 || rounds[0].UserFindingsJSON == nil {
					t.Fatalf("selection not persisted: %+v, %v", rounds, err)
				}
				selected, err := types.ParseFindingsJSON(*rounds[0].UserFindingsJSON)
				if err != nil || len(selected.Items) != 1 || selected.Items[0].ID != selectedID || selected.Items[0].Description != descriptions[selectedIndex] || selected.Items[0].UserInstructions != instruction {
					t.Errorf("durable decision includes an unselected finding: %+v, %v", selected, err)
				}
			}
		})
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

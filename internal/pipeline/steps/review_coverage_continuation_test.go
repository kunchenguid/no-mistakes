package steps

import (
	"context"
	"database/sql"
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

type coverageContinuationTestGate struct{}

func (*coverageContinuationTestGate) Name() types.StepName { return types.StepTest }
func (*coverageContinuationTestGate) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"required-test","severity":"warning","description":"Keep the next validation gate pending","action":"ask-user"}]}`}, nil
}

// The reviewer here is the real ReviewStep. Only the agent boundary is
// scripted, so starting a new full review cannot masquerade as continuation.
func TestReviewStep_ParkedCoverageContinuationTargetsOnlyRetainedGap(t *testing.T) {
	t.Parallel()
	testRetainedCoverageContinuation(t, false)
}

func TestReviewStep_RecoveredCoverageContinuationTargetsOnlyRetainedGap(t *testing.T) {
	t.Parallel()
	testRetainedCoverageContinuation(t, true)
}

func testRetainedCoverageContinuation(t *testing.T, recovered bool) {
	testRetainedCoverageCase(t, recovered, "complete")
}

func TestReviewStep_ContinuationDoesNotWaiveMissingCoverageOrNewFindings(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"partial", "new finding", "new question", "out-of-scope output"} {
		t.Run(mode, func(t *testing.T) { testRetainedCoverageCase(t, false, mode) })
	}
}

func TestReviewStep_ContinuationRefusesUnboundRecordsAndChangedSources(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"record:missing", "record:malformed", "record:version", "record:complete", "record:outside", "record:run", "record:step", "record:head", "record:base", "record:trusted", "record:scope", "trusted change", "scope change", "base change", "head change", "dirty worktree"} {
		t.Run(mode, func(t *testing.T) { testRetainedCoverageCase(t, false, mode) })
	}
}

func TestReviewStep_ContinuationCannotCertifySourceChangedDuringItsTurn(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"head changed during turn", "dirty during turn"} {
		t.Run(mode, func(t *testing.T) { testRetainedCoverageCase(t, false, mode) })
	}
}

func testRetainedCoverageCase(t *testing.T, recovered bool, mode string) {
	dir, base, _ := twoFileRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "feature3.txt"), []byte("third changed file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature3.txt")
	gitCmd(t, dir, "commit", "-m", "third changed file")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte(".tmp-evidence/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".tmp-evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(dir, ".tmp-evidence", "retained.txt")
	if err := os.WriteFile(evidencePath, []byte("retained original evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkEvidence := func() {
		b, err := os.ReadFile(evidencePath)
		if err != nil || string(b) != "retained original evidence\n" {
			t.Fatalf("continuation lost ignored evidence: %q %v", b, err)
		}
	}
	var conversationDir string
	ag := &mockAgent{name: "coverage-continuation-script"}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		n := len(ag.calls)
		capture, _ := json.Marshal(map[string]any{"invocation": n, "purpose": opts.Purpose, "prompt": opts.Prompt, "cwd": opts.CWD})
		t.Logf("ACTUAL_REVIEW_AGENT %s", capture)
		switch n {
		case 1:
			return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{"feature.txt"}))}, nil
		case 2:
			return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{"feature2.txt"}))}, nil
		case 3:
			if opts.Purpose != "review-coverage" || !strings.Contains(opts.Prompt, coverageCompletionMarker) {
				return nil, fmt.Errorf("retained coverage was forgotten: expected missing-path completion, got purpose %q", opts.Purpose)
			}
			section := strings.SplitN(opts.Prompt, coverageCompletionMarker, 2)[1]
			if !strings.Contains(section, "  - feature3.txt\n") || strings.Contains(section, "  - feature.txt\n") || strings.Contains(section, "  - feature2.txt\n") {
				return nil, fmt.Errorf("continuation did not focus only the retained gap: %s", section)
			}
			output := coverageFindingJSON([]string{"feature3.txt"})
			switch mode {
			case "partial":
				output = coverageFindingJSON(nil)
			case "out-of-scope output":
				output = coverageFindingJSON([]string{"not-a-changed-file.txt"})
			case "new finding":
				output = `{"findings":[{"id":"new-defect","file":"feature3.txt","line":1,"severity":"error","description":"new substantive defect","action":"ask-user"}],"reviewed_paths":["feature3.txt"],"risk_level":"high","risk_rationale":"new substantive defect","risk_scope":"source-or-external"}`
			case "new question":
				if err := appendAgentQuestionLine(conversationDir, `{"id":"q-continuation","kind":"question","question":"Is feature3.txt's fallback intended?","options":["yes","no"],"weight":"major","file":"feature3.txt","line":1}`); err != nil {
					return nil, err
				}
			case "head changed during turn", "dirty during turn":
				if err := os.WriteFile(filepath.Join(dir, "feature3.txt"), []byte("changed during review\n"), 0o600); err != nil {
					return nil, err
				}
				if mode == "head changed during turn" {
					gitCmd(t, dir, "commit", "-am", "changed during review")
				}
			}
			return &agent.Result{Output: json.RawMessage(output)}, nil
		default:
			return nil, fmt.Errorf("unexpected agent invocation %d", n)
		}
	}
	ensureHermeticOrigin(t, dir)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(dir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{DB: database, Run: run, Repo: repo, WorkDir: dir, EvidenceDir: p.RunEvidenceDir("", run.ID), Config: &config.Config{Agent: types.AgentClaude, TrustedConfigSHA: base}}
	if mode == "new question" {
		withReviewConversation(sctx)
		conversationDir = reviewConversationDir(sctx)
	}
	t.Cleanup(func() { _ = sctx.DB.Close() })
	executor := pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, []pipeline.Step{&ReviewStep{}, &coverageContinuationTestGate{}}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); done <- executor.Execute(ctx, sctx.Run, sctx.Repo, dir) }()
	t.Cleanup(func() {
		cancel(pipeline.ErrDaemonShutdown)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("disposable review executor did not exit")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	var binding *ipc.CoverageBinding
	for time.Now().Before(deadline) {
		steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) > 0 && steps[0].Status == types.StepStatusAwaitingApproval {
			rounds, err := sctx.DB.GetRoundsByStep(steps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 1 || rounds[0].FindingsJSON == nil {
				t.Fatalf("missing actual persisted prior review: %+v", rounds)
			}
			prior, err := parseReviewAnalyzerOutput(&agent.Result{Output: json.RawMessage(*rounds[0].FindingsJSON)})
			if err != nil || len(prior.ReviewedPaths) != 2 {
				t.Fatalf("prior review did not retain exactly its two reviewed files: %+v %v", prior, err)
			}
			t.Logf("STORED_PRIOR_COVERAGE run=%s step=%s round=%s head=%s base=%s trusted=%s findings=%s", sctx.Run.ID, steps[0].ID, rounds[0].ID, head, base, sctx.Config.TrustedConfigSHA, *rounds[0].FindingsJSON)
			binding = &ipc.CoverageBinding{HeadSHA: head, StepResultID: steps[0].ID, RoundID: rounds[0].ID}
			break
		}
		select {
		case err := <-done:
			t.Fatalf("review exited before the coverage gate: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if binding == nil {
		t.Fatal("real ReviewStep did not park after two partial turns")
	}
	if recovered {
		cancel(pipeline.ErrDaemonShutdown)
		if err := <-done; !errors.Is(err, pipeline.ErrParkedAtShutdown) {
			t.Fatalf("disposable executor did not preserve the parked round: %v", err)
		}
		<-finished
		if err := sctx.DB.Close(); err != nil {
			t.Fatal(err)
		}
		sctx.DB, err = db.Open(p.DB())
		if err != nil {
			t.Fatal(err)
		}
		sctx.Run, err = sctx.DB.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		executor = pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, []pipeline.Step{&ReviewStep{}, &coverageContinuationTestGate{}}, nil)
		ctx, cancel = context.WithCancelCause(context.Background())
		defer cancel(pipeline.ErrDaemonShutdown)
		done = make(chan error, 1)
		finished = make(chan struct{})
		go func() { defer close(finished); done <- executor.Resume(ctx, sctx.Run, sctx.Repo, dir) }()
		deadline = time.Now().Add(5 * time.Second)
	}
	expectRefusal := false
	expectScopePark := mode == "scope change" || mode == "base change" || mode == "record:scope"
	switch mode {
	case "trusted change":
		sctx.Config.TrustedConfigSHA = "different-trusted-config"
		expectRefusal = true
	case "scope change":
		sctx.Config.IgnorePatterns = []string{"feature3.txt"}
	case "base change":
		gitCmd(t, dir, "push", "origin", head+":refs/heads/main")
	case "head change", "dirty worktree":
		if err := os.WriteFile(filepath.Join(dir, "feature3.txt"), []byte("out-of-band change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if mode == "head change" {
			gitCmd(t, dir, "commit", "-am", "different head")
		}
		expectRefusal = true
	}
	if strings.HasPrefix(mode, "record:") {
		rounds, err := sctx.DB.GetRoundsByStep(binding.StepResultID)
		if err != nil || len(rounds) != 1 {
			t.Fatalf("read fixture source round: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(*rounds[0].FindingsJSON), &body); err != nil {
			t.Fatal(err)
		}
		scope := body["review_coverage_scope"].(map[string]any)
		switch mode {
		case "record:missing":
			delete(body, "review_coverage_scope")
		case "record:malformed":
			body["review_coverage_scope"] = "invalid stored scope"
		case "record:version":
			scope["version"] = 99
		case "record:complete":
			body["reviewed_paths"] = []string{"feature.txt", "feature2.txt", "feature3.txt"}
		case "record:outside":
			body["reviewed_paths"] = []string{"different-path.txt"}
		case "record:run":
			scope["run_id"] = "different-run"
		case "record:step":
			scope["step_result_id"] = "different-step"
		case "record:head":
			scope["head_sha"] = "different-head"
		case "record:base":
			scope["run_base_sha"] = "different-base"
		case "record:trusted":
			scope["trusted_config_sha"] = "different-trusted-config"
		case "record:scope":
			scope["paths"] = []string{"feature.txt", "feature2.txt", "different-path.txt"}
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		// The private persisted input is intentionally corrupted. Assertions use
		// the real public gate response and real review consumer, not SQL tokens.
		fixture, err := sql.Open("sqlite", p.DB())
		if err != nil {
			t.Fatal(err)
		}
		_, err = fixture.Exec("UPDATE step_rounds SET findings_json = ? WHERE id = ?", string(encoded), binding.RoundID)
		_ = fixture.Close()
		if err != nil {
			t.Fatal(err)
		}
		expectRefusal = mode != "record:scope"
	}
	for {
		_, err := executor.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", pipeline.ResponseOptions{Coverage: binding})
		if expectRefusal && err != nil && !strings.Contains(err.Error(), "no step awaiting approval") {
			steps, readErr := sctx.DB.GetStepsByRun(run.ID)
			if readErr != nil || len(steps) != 2 || steps[0].Status != types.StepStatusAwaitingApproval || steps[1].Status != types.StepStatusPending || len(ag.calls) != 2 {
				t.Fatalf("invalid continuation consumed its gate or called the agent: %+v calls=%d err=%v", steps, len(ag.calls), readErr)
			}
			checkEvidence()
			return
		}
		if err == nil {
			if expectRefusal {
				t.Fatal("invalid provenance/source continuation was accepted")
			}
			break
		}
		if !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
			t.Fatalf("bound continuation refused: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if mode != "complete" && len(steps) == 2 && steps[0].Status == types.StepStatusAwaitingApproval {
			rounds, readErr := sctx.DB.GetRoundsByStep(binding.StepResultID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(rounds) == 2 {
				wantCalls := 3
				if expectScopePark {
					wantCalls = 2
				}
				if steps[1].Status != types.StepStatusPending || len(ag.calls) != wantCalls || rounds[0].ID != binding.RoundID || rounds[1].SelectionSource != nil {
					t.Fatalf("invalid/incomplete coverage bypassed the required gate: %+v calls=%d", steps, len(ag.calls))
				}
				if rounds[1].FindingsJSON == nil {
					t.Fatal("parked continuation lost its refusal/findings envelope")
				}
				f, parseErr := types.ParseFindingsJSON(*rounds[1].FindingsJSON)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				if mode == "partial" && (len(f.Items) != 0 || len(f.ReviewedPaths) != 2 || !pipeline.CanContinueReviewCoverage(types.StepReview, *rounds[1].FindingsJSON)) {
					t.Fatalf("partial coverage was discarded or certified: %+v", f)
				}
				if mode == "new finding" && (len(f.Items) != 1 || f.Items[0].Description != "new substantive defect" || f.Items[0].ActionOrDefault() != types.ActionAskUser) {
					t.Fatalf("continuation lost the new actionable finding: %+v", f.Items)
				}
				if mode == "new question" && len(questionFindings(t, *rounds[1].FindingsJSON)) != 1 {
					t.Fatal("continuation lost the real written review question")
				}
				if mode != "partial" && pipeline.CanContinueReviewCoverage(types.StepReview, *rounds[1].FindingsJSON) {
					t.Fatal("invalid scope, findings or questions advertised a coverage-only release")
				}
				checkEvidence()
				return
			}
		}
		if len(steps) == 2 && steps[0].Status == types.StepStatusCompleted && steps[1].Status == types.StepStatusAwaitingApproval {
			if mode != "complete" {
				t.Fatalf("continuation certified source which must remain gated: %s", mode)
			}
			rounds, err := sctx.DB.GetRoundsByStep(binding.StepResultID)
			if err != nil || len(rounds) != 2 || rounds[0].ID != binding.RoundID || rounds[1].Trigger != "coverage" || rounds[1].SelectionSource != nil {
				t.Fatalf("continuation lost round identity or recorded a waiver: %+v %v", rounds, err)
			}
			if len(ag.calls) != 3 {
				t.Fatalf("agent invocations=%d, want two original partial turns plus one focused continuation", len(ag.calls))
			}
			if rounds[1].FindingsJSON == nil {
				t.Fatal("continued round lost its provenance")
			}
			findings, err := types.ParseFindingsJSON(*rounds[1].FindingsJSON)
			if err != nil || findings.ReviewCoverageScope == nil || findings.ReviewCoverageScope.SourceRoundID != binding.RoundID {
				t.Fatalf("continued coverage is not bound to its stored source round: %+v %v", findings.ReviewCoverageScope, err)
			}
			checkEvidence()
			return
		}
		select {
		case err := <-done:
			t.Fatalf("real continuation failed instead of completing retained coverage: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("continuation did not complete review and retain the next Test gate")
}

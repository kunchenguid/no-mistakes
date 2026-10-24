package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AgentCrashRetryDoesNotRetryGitExit(t *testing.T) {
	database, p, _, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	head, err := git.HeadSHA(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "git-failure", head, head)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ag := &hangingAgent{name: "unused", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{}, nil
	}}
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := git.Run(sctx.Ctx, sctx.WorkDir, "fetch", "missing-remote", "main")
		return &StepOutcome{}, err
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, ag, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, work); err == nil || calls != 0 {
		t.Fatalf("git failure retried as an agent crash: calls=%d error=%v", calls, err)
	}
}

func TestExecutor_AgentCrashRetryFirstPushUsesResolvedBase(t *testing.T) {
	database, p, _, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	head, err := git.HeadSHA(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "first-push", head, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ag := &hangingAgent{name: "first-push", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			return nil, processExitError{"agent process exited: exit status 1"}
		}
		return &agent.Result{}, nil
	}}
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if err := sctx.CheckAgentCrashRetryBase(head); err != nil {
			return nil, err
		}
		_, err := sctx.RunAgent(agent.RunOpts{})
		return &StepOutcome{}, err
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, ag, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, work); err != nil || calls != 2 {
		t.Fatalf("first-push crash retry = %v, calls=%d; want success after one retry", err, calls)
	}
}

func TestExecutor_AgentCrashRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retries    int
		failures   int
		wantCalls  int
		wantFailed bool
		direct     bool
	}{
		{"crash then success", 1, 1, 2, false, false},
		{"exhausted", 1, 2, 2, true, false},
		{"three retry bound", 3, 4, 4, true, false},
		{"default unchanged", 0, 1, 1, true, false},
		{"direct agent call", 1, 1, 2, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database, p, _, repo := setupTest(t)
			work := t.TempDir()
			initGitRepo(t, work)
			head, err := git.HeadSHA(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "crash", head, head)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			ag := &hangingAgent{name: "crashing", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls <= tc.failures {
					return nil, processExitError{"agent process exited: exit status 1"}
				}
				return &agent.Result{}, nil
			}}
			before := newPassStep(types.StepReview)
			after := newPassStep(types.StepLint)
			crash := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				call := sctx.RunAgent
				if tc.direct {
					call = func(opts agent.RunOpts) (*agent.Result, error) { return sctx.Agent.Run(sctx.Ctx, opts) }
				}
				_, err := call(agent.RunOpts{})
				return &StepOutcome{}, err
			}}
			executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: tc.retries}, ag, []Step{before, crash, after}, nil)
			events := collectEvents(executor)
			execErr := executor.Execute(context.Background(), run, repo, work)
			if (execErr != nil) != tc.wantFailed || calls != tc.wantCalls {
				t.Fatalf("Execute = %v, calls = %d; want failed=%v calls=%d", execErr, calls, tc.wantFailed, tc.wantCalls)
			}
			if before.callCount() != 1 || after.callCount() != boolInt(!tc.wantFailed) {
				t.Fatalf("earlier/later calls = %d/%d", before.callCount(), after.callCount())
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || len(steps) != 3 || steps[0].Status != types.StepStatusCompleted {
				t.Fatalf("completed step lost: %+v, %v", steps, err)
			}
			failedEvents := 0
			for _, event := range events.all() {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusFailed) {
					failedEvents++
				}
			}
			if failedEvents != boolInt(tc.wantFailed) {
				t.Fatalf("intermediate crash emitted a terminal event: %d", failedEvents)
			}
			if tc.wantFailed {
				if steps[1].FailureCause == nil || *steps[1].FailureCause != db.StepFailureCauseAgentInvocation {
					t.Fatalf("exhausted invocation lost manual continuation cause: %+v", steps[1])
				}
				const wantError = "step test failed: agent process exited: exit status 1"
				if execErr.Error() != wantError || errString(executor, run.ID) != wantError {
					t.Fatalf("failure output changed: %v / %s", execErr, errString(executor, run.ID))
				}
			}
		})
	}
}

// Exercise Claude's real subprocess and transient-retry loop, not a synthetic
// error with the same diagnostic. The fake executable is not race-instrumented.
func TestExecutor_AgentCrashRetryDoesNotRetryExhaustedClaude(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "claude.go")
	bin := filepath.Join(dir, "claude.exe")
	attempts := filepath.Join(dir, "attempts")
	const fakeClaude = `package main
import (
 "fmt"
 "os"
)
func main() {
 f, err := os.OpenFile(os.Getenv("NM_TEST_CLAUDE_ATTEMPTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
 if err != nil { panic(err) }
 if _, err := f.WriteString("attempt\n"); err != nil { panic(err) }
 if err := f.Close(); err != nil { panic(err) }
 fmt.Println("{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"api_error_status\":503,\"result\":\"service unavailable\"}")
 os.Exit(1)
}
`
	if err := os.WriteFile(source, []byte(fakeClaude), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("go", "build", "-o", bin, source).CombinedOutput(); err != nil {
		t.Fatalf("build fake Claude: %v\n%s", err, output)
	}
	ag, err := agent.New(types.AgentClaude, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	database, p, _, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	head, err := git.HeadSHA(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "exhausted-claude", head, head)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		_, err := sctx.RunAgent(agent.RunOpts{CWD: work, Prompt: "test", Env: []string{"NM_TEST_CLAUDE_ATTEMPTS=" + attempts}})
		return &StepOutcome{}, err
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, ag, []Step{step}, nil)
	execErr := executor.Execute(context.Background(), run, repo, work)
	const diagnostic = "step test failed: claude exited: exit status 1: API error status 503: service unavailable"
	if execErr == nil || execErr.Error() != diagnostic || errString(executor, run.ID) != diagnostic {
		t.Fatalf("original diagnostic lost: error=%v persisted=%s", execErr, errString(executor, run.ID))
	}
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || string(data) != strings.Repeat("attempt\n", 4) {
		t.Fatalf("adapter exhaustion spent crash retries: step calls=%d subprocess attempts=%q", calls, data)
	}
}

func TestExecutor_AgentCrashRetryDoesNotRetryRejectedCodexAfterFallbackCrash(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "codex.go")
	bin := filepath.Join(dir, "codex.exe")
	attempts := filepath.Join(dir, "attempts")
	const fakeCodex = `package main
import (
 "fmt"
 "os"
)
func main() {
 f, err := os.OpenFile(os.Getenv("NM_TEST_CODEX_ATTEMPTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
 if err != nil { panic(err) }
 if _, err := f.WriteString("attempt\n"); err != nil { panic(err) }
 if err := f.Close(); err != nil { panic(err) }
 fmt.Fprintln(os.Stderr, "Invalid json schema: unsupported array")
 os.Exit(1)
}
`
	if err := os.WriteFile(source, []byte(fakeCodex), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("go", "build", "-o", bin, source).CombinedOutput(); err != nil {
		t.Fatalf("build fake Codex: %v\n%s", err, output)
	}
	codex, err := agent.New(types.AgentCodex, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondCalls := 0
	second := &hangingAgent{name: "second", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		secondCalls++
		return nil, processExitError{"second exited: fallback crash diagnostic"}
	}}
	database, p, _, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	head, err := git.HeadSHA(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "codex-fallback-crash", head, head)
	if err != nil {
		t.Fatal(err)
	}
	stepCalls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		stepCalls++
		_, err := sctx.RunAgent(agent.RunOpts{
			CWD: work, Prompt: "review",
			JSONSchema: []byte(`{"type":"object","properties":{"findings":{"type":"array"}}}`),
			Env:        []string{"NM_TEST_CODEX_ATTEMPTS=" + attempts},
		})
		if !agent.IsTerminalProviderError(err) {
			t.Errorf("RunAgent error = %v, want terminal provider error", err)
		}
		return &StepOutcome{}, err
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, agent.NewFallback([]agent.Agent{codex, second}), []Step{step}, nil)
	execErr := executor.Execute(context.Background(), run, repo, work)
	for _, diagnostic := range []string{"codex structured-output incompatibility", "second exited: fallback crash diagnostic"} {
		if execErr == nil || !strings.Contains(execErr.Error(), diagnostic) || !strings.Contains(errString(executor, run.ID), diagnostic) {
			t.Errorf("diagnostic %q lost: error=%v persisted=%s", diagnostic, execErr, errString(executor, run.ID))
		}
	}
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if stepCalls != 1 || secondCalls != 1 || string(data) != "attempt\n" {
		t.Fatalf("schema rejection spent crash retries: step calls=%d fallback calls=%d Codex attempts=%q", stepCalls, secondCalls, data)
	}
}

func TestExecutor_AgentCrashRetryAfterTerminalClaudeAndCodexCrash(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "claude.go")
	bin := filepath.Join(dir, "claude.exe")
	attempts := filepath.Join(dir, "attempts")
	const fakeClaude = `package main
import (
 "fmt"
 "os"
)
func main() {
 f, err := os.OpenFile(os.Getenv("NM_TEST_CLAUDE_ATTEMPTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
 if err != nil { panic(err) }
 if _, err := f.WriteString("attempt\n"); err != nil { panic(err) }
 if err := f.Close(); err != nil { panic(err) }
 fmt.Println("{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"api_error_status\":400,\"result\":\"invalid request\"}")
 os.Exit(1)
}
`
	if err := os.WriteFile(source, []byte(fakeClaude), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("go", "build", "-o", bin, source).CombinedOutput(); err != nil {
		t.Fatalf("build fake Claude: %v\n%s", err, output)
	}
	claude, err := agent.New(types.AgentClaude, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondCalls := 0
	codex := &hangingAgent{name: "codex", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		secondCalls++
		return nil, processExitError{"codex exited: fallback crash diagnostic"}
	}}
	database, p, _, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	head, err := git.HeadSHA(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "claude-fallback-crash", head, head)
	if err != nil {
		t.Fatal(err)
	}
	stepCalls := 0
	terminalClaudeAttempts := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		stepCalls++
		_, err := sctx.RunAgent(agent.RunOpts{
			CWD: work, Prompt: "review",
			Env: []string{"NM_TEST_CLAUDE_ATTEMPTS=" + attempts},
			OnAttempt: func(attempt agent.Attempt) {
				if attempt.Agent == "claude" {
					if !agent.IsTerminalProviderError(attempt.Err) {
						t.Errorf("Claude error = %v, want terminal provider error", attempt.Err)
					} else {
						terminalClaudeAttempts++
					}
				}
			},
		})
		if agent.IsTerminalProviderError(err) {
			t.Errorf("RunAgent error = %v, want only the fallback crash", err)
		}
		return &StepOutcome{}, err
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, agent.NewFallback([]agent.Agent{claude, codex}), []Step{step}, nil)
	execErr := executor.Execute(context.Background(), run, repo, work)
	const diagnostic = "step review failed: codex exited: fallback crash diagnostic"
	if execErr == nil || execErr.Error() != diagnostic || errString(executor, run.ID) != diagnostic {
		t.Fatalf("fallback diagnostic changed: error=%v persisted=%s", execErr, errString(executor, run.ID))
	}
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if stepCalls != 2 || secondCalls != 2 || terminalClaudeAttempts != 2 || string(data) != "attempt\nattempt\n" {
		t.Fatalf("fallback crash did not use its retry: step calls=%d Codex calls=%d terminal Claude attempts=%d subprocess attempts=%q", stepCalls, secondCalls, terminalClaudeAttempts, data)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func errString(executor *Executor, runID string) string {
	run, _ := executor.db.GetRun(runID)
	if run != nil && run.Error != nil {
		return *run.Error
	}
	return ""
}

func TestExecutor_AgentCrashRetryRefusesChangedProof(t *testing.T) {
	for _, change := range []string{"head", "run head", "base", "config", "evidence", "evidence contents", "completed evidence", "dirty", "ignored", "initial dirty"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			database, p, _, repo := setupTest(t)
			work := t.TempDir()
			initGitRepo(t, work)
			head, err := git.HeadSHA(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "proof", head, head)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{AgentCrashRetries: 1}
			before := newPassStep(types.StepReview)
			calls := 0
			crash := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				switch change {
				case "head":
					execGit(t, work, "commit", "--allow-empty", "-m", "agent commit")
				case "run head":
					if err := database.UpdateRunHeadSHA(run.ID, "changed"); err != nil {
						t.Fatal(err)
					}
				case "base":
					sctx.Run.BaseSHA = "changed"
				case "config":
					cfg.TrustedConfigSHA = "changed"
				case "evidence", "evidence contents":
					if err := os.MkdirAll(sctx.EvidenceDir, 0o700); err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, sctx.EvidenceDir, "partial.txt", "partial evidence")
				case "completed evidence":
					steps, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.SetStepFindings(steps[0].ID, `{"findings":[]}`); err != nil {
						t.Fatal(err)
					}
				case "dirty":
					writeTestFile(t, work, "README.md", "unfinished")
				case "ignored":
					execGit(t, work, "config", "core.excludesFile", filepath.Join(work, ".git", "local-ignore"))
					writeTestFile(t, filepath.Join(work, ".git"), "local-ignore", "partial.txt\n")
					writeTestFile(t, work, "partial.txt", "unfinished")
				}
				return nil, &agentInvocationFailure{err: processExitError{"agent process crashed"}}
			}}
			if change == "initial dirty" {
				writeTestFile(t, work, "README.md", "already dirty")
			}
			executor := NewExecutor(database, p, cfg, nil, []Step{before, crash}, nil)
			if change == "evidence contents" {
				evidence := executor.runEvidenceDir(run.ID)
				if err := os.MkdirAll(evidence, 0o700); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, evidence, "partial.txt", "earlier evidence")
			}
			if err := executor.Execute(context.Background(), run, repo, work); err == nil || calls != 1 {
				t.Fatalf("changed proof retried: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestExecutor_AgentCrashRetryDoesNotRetryVerdicts(t *testing.T) {
	t.Parallel()
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepTest, `{"findings":[{"id":"test-1","severity":"error","description":"live scenario failed","action":"auto-fix"}],"verdict":"no-go"}`)
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 3}, nil, []Step{step}, nil)
	executor.onEvent = func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepTest, types.ActionAbort, nil); err != nil {
				t.Error(err)
			}
		}
	}
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil || step.callCount() != 1 {
		t.Fatalf("verdict retried: calls=%d err=%v", step.callCount(), err)
	}
}

func TestCrashRetryPhases(t *testing.T) {
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepPush, types.StepPR, types.StepCI, "gate.test.check"} {
		if crashRetryStep(name) {
			t.Fatalf("unsafe phase %s can be replayed", name)
		}
	}
	for _, phase := range []string{"fix", "answer", "fix-answer", "coverage"} {
		t.Run(phase, func(t *testing.T) {
			sctx := &StepContext{
				Fixing:            phase == "fix" || phase == "fix-answer",
				FinalizingAnswers: phase == "answer" || phase == "fix-answer",
			}
			if phase == "coverage" {
				sctx.CoverageContinuation = &db.StepRound{}
			}
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				if phase != "fix" {
					if err := sctx.CheckAgentCrashRetryBase("advanced"); err != nil {
						return nil, err
					}
				}
				return nil, &agentInvocationFailure{err: processExitError{"crash"}}
			}}
			executor := &Executor{crashRetryBaseSHA: "original"}
			budget := 3
			_, err := executor.executeWithCrashRetry(step, sctx, &budget)
			if err == nil || calls != 1 || budget != 3 {
				t.Fatalf("%s replayed: calls=%d budget=%d err=%v", phase, calls, budget, err)
			}
			if phase != "fix" && !strings.Contains(err.Error(), "effective validation base changed") {
				t.Fatalf("%s ignored the changed effective base: %v", phase, err)
			}
		})
	}
}

func TestExecutor_ReviewContinuationAfterDefaultBranchAdvanceFailsClosed(t *testing.T) {
	base := "default-before"
	executor := &Executor{crashRetryBaseSHA: base}
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		return nil, sctx.CheckAgentCrashRetryBase("default-after")
	}}
	sctx := &StepContext{FinalizingAnswers: true}
	budget := 1
	_, err := executor.executeWithCrashRetry(step, sctx, &budget)
	if err == nil || !strings.Contains(err.Error(), "effective validation base changed") || calls != 1 || budget != 1 {
		t.Fatalf("review continuation crossed the advanced default branch: calls=%d budget=%d err=%v", calls, budget, err)
	}
}

func seedRecoveredTestGate(t *testing.T, database *db.DB, run *db.Run) *db.Run {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(review.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	testStep, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(testStep.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"test-1","severity":"warning","description":"operator decision","action":"ask-user"}]}`
	if err := database.ParkStepForApproval(run.ID, testStep.ID, types.StepStatusAwaitingApproval, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(testStep.ID, 1, "initial", &findings, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepDocument); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestExecutor_AgentCrashRetryDisabledAfterRecovery(t *testing.T) {
	database, p, run, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	run = seedRecoveredTestGate(t, database, run)
	calls := 0
	document := &adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		return nil, &agentInvocationFailure{err: processExitError{"agent crashed"}}
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, nil, []Step{newPassStep(types.StepReview), newPassStep(types.StepTest), document}, nil)
	executor.onEvent = func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
				t.Error(err)
			}
		}
	}
	err := executor.Resume(context.Background(), run, repo, work)
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "step document failed: agent crashed") {
		t.Fatalf("recovered crash was not an ordinary single failure: calls=%d err=%v", calls, err)
	}
}

func TestExecutor_RecoveredContinuationDoesNotFalseFailBaseFence(t *testing.T) {
	database, p, run, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	run = seedRecoveredTestGate(t, database, run)
	calls := 0
	document := &adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
		calls++
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, &config.Config{AgentCrashRetries: 1}, nil, []Step{newPassStep(types.StepReview), newPassStep(types.StepTest), document}, nil)
	executor.onEvent = func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
				t.Error(err)
			}
		}
	}
	if err := executor.Resume(context.Background(), run, repo, work); err != nil || calls != 1 {
		t.Fatalf("recovered successful continuation failed: calls=%d err=%v", calls, err)
	}
}

func TestAgentCrashRetryPinsTheEffectiveBase(t *testing.T) {
	base := ""
	first := &StepContext{crashRetryBaseSHA: &base}
	second := &StepContext{crashRetryBaseSHA: &base}
	if err := first.CheckAgentCrashRetryBase("original"); err != nil {
		t.Fatal(err)
	}
	if err := second.CheckAgentCrashRetryBase("original"); err != nil {
		t.Fatal(err)
	}
	if err := second.CheckAgentCrashRetryBase("changed"); err == nil {
		t.Fatal("later validation reused completed steps on a different effective base")
	}
	if err := (&StepContext{}).CheckAgentCrashRetryBase("changed"); err != nil {
		t.Fatal("unconfigured base resolution changed")
	}
}

type usageLimitReachedError struct{}

func (usageLimitReachedError) Error() string { return "provider usage limit reached" }

func (usageLimitReachedError) UsageLimitReached() bool { return true }

type terminalProviderError struct{}

func (terminalProviderError) Error() string { return "provider authentication failed" }

func (terminalProviderError) TerminalProviderError() bool { return true }

type processExitError struct{ message string }

func (e processExitError) Error() string { return e.message }

func (processExitError) Unwrap() error { return &exec.ExitError{} }

func TestRetryableAgentCrash(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		configured bool
		want       bool
	}{
		{"process exit", &agentInvocationFailure{err: processExitError{"agent exited: exit status 1"}}, false, true},
		{"terminal provider error with process exit", &agentInvocationFailure{err: errors.Join(terminalProviderError{}, processExitError{"claude exited: exit status 1"})}, false, false},
		{"unowned process exit", &exec.ExitError{}, false, false},
		{"unclassified error", &agentInvocationFailure{err: errors.New("exit status 1")}, false, false},
		{"unparseable", agent.RejectStructuredOutput(errors.New("invalid JSON")), false, false},
		{"structured output field names", &agentInvocationFailure{err: agent.RejectStructuredOutput(errors.New(`structured output: {"author":"token"}`))}, false, false},
		{"auth", &agentInvocationFailure{err: errors.New("provider authentication failed")}, false, false},
		{"login", &agentInvocationFailure{err: errors.New("login required")}, false, false},
		{"api key", &agentInvocationFailure{err: errors.New("invalid API key")}, false, false},
		{"credit", &agentInvocationFailure{err: errors.New("insufficient credit balance")}, false, false},
		{"forbidden", &agentInvocationFailure{err: errors.New("403 forbidden")}, false, false},
		{"quota", &agentInvocationFailure{err: errors.New("insufficient_quota")}, false, false},
		{"claude schema exhaustion", &agentInvocationFailure{err: errors.New("error_max_structured_output_retries")}, false, false},
		{"default hang", &agentInvocationError{bound: "silent budget", cause: ErrAgentTimeout, adapter: context.Canceled}, false, true},
		{"quota beyond bounded report", &agentInvocationError{message: "timed out; bounded adapter report", bound: "silent budget", cause: ErrAgentTimeout, adapter: errors.Join(usageLimitReachedError{}, errors.New(strings.Repeat("x", 500)+" insufficient_quota"))}, false, false},
		{"Claude session limit", &agentInvocationFailure{err: errors.New("claude exited: API error status 429: You've hit your session limit · resets later")}, false, false},
		{"adapter usage limit signal", &agentInvocationFailure{err: usageLimitReachedError{}}, false, false},
		{"configured timeout", &agentInvocationError{bound: "silent budget", cause: ErrAgentTimeout}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableAgentCrash(context.Background(), tc.err, tc.configured); got != tc.want {
				t.Fatalf("retry = %v want %v", got, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if retryableAgentCrash(ctx, &agentInvocationFailure{err: processExitError{"crash"}}, false) {
		t.Fatal("cancelled run retried")
	}
}

func TestExecutor_AgentCrashRetryUsesGeneratedDefaultConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global.yaml")
	config.EnsureDefaultGlobalConfig(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "agent_crash_retries: 0", "agent_crash_retries: 1", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	if cfg.AgentCrashRetries != 1 || cfg.AgentTimeoutsConfigured {
		t.Fatalf("generated defaults changed retry eligibility: retries=%d configured=%t", cfg.AgentCrashRetries, cfg.AgentTimeoutsConfigured)
	}

	database, p, _, repo := setupTest(t)
	work := t.TempDir()
	initGitRepo(t, work)
	head, err := git.HeadSHA(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "generated-default-hang", head, head)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ag := &hangingAgent{name: "generated-default-hang", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("generated-default invocation has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining < config.DefaultAgentTimeout-time.Second || remaining > config.DefaultAgentTimeout {
				t.Fatalf("generated-default deadline has %s remaining; want %s", remaining, config.DefaultAgentTimeout)
			}
			return nil, &agentInvocationError{
				message: "agent timed out at the silent budget",
				bound:   "silent budget; no still-working cap is set",
				cause:   ErrAgentTimeout,
			}
		}
		return &agent.Result{}, nil
	}}
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.RunAgent(agent.RunOpts{})
		return &StepOutcome{}, err
	}}
	executor := NewExecutor(database, p, cfg, ag, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, work); err != nil || calls != 2 {
		t.Fatalf("generated default-budget hang = %v, calls=%d; want one retry and two calls", err, calls)
	}
}

func TestExecutor_AgentCrashRetryHang(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "default budget", true: "operator budget"}[configured], func(t *testing.T) {
			t.Parallel()
			database, p, _, repo := setupTest(t)
			work := t.TempDir()
			initGitRepo(t, work)
			head, err := git.HeadSHA(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "hang", head, head)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			ag := &hangingAgent{name: "hang", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 {
					<-ctx.Done() // the invocation's own cancellation proves the hang ended
					return nil, ctx.Err()
				}
				return &agent.Result{}, nil
			}}
			step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				_, err := sctx.RunAgent(agent.RunOpts{})
				return &StepOutcome{}, err
			}}
			cfg := &config.Config{AgentCrashRetries: 1, AgentTimeout: time.Second, AgentTimeoutsConfigured: configured}
			executor := NewExecutor(database, p, cfg, ag, []Step{step}, nil)
			err = executor.Execute(context.Background(), run, repo, work)
			if (err != nil) != configured || calls != 2-boolInt(configured) {
				t.Fatalf("calls=%d err=%v configured=%v", calls, err, configured)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || steps[0].FailureCause != nil {
				t.Fatalf("timeout became a manual continuation cause: steps=%+v error=%v", steps, err)
			}
		})
	}
}

// A repository that only tightens review.agent_timeout configured a budget for
// Review alone: a Review budget cut is a deliberate stop and is not retried,
// while Document, Lint, and Test keep the retry their untouched budgets earn.
func TestExecutor_AgentCrashRetryRepoReviewTimeoutScopedToReview(t *testing.T) {
	for _, stepName := range []types.StepName{types.StepReview, types.StepDocument, types.StepLint, types.StepTest} {
		t.Run(string(stepName), func(t *testing.T) {
			t.Parallel()
			database, p, _, repo := setupTest(t)
			work := t.TempDir()
			initGitRepo(t, work)
			head, err := git.HeadSHA(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "hang", head, head)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			ag := &hangingAgent{name: "hang", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return &agent.Result{}, nil
			}}
			step := &adaptiveCallStep{name: stepName, fn: func(sctx *StepContext) (*StepOutcome, error) {
				_, err := sctx.RunAgent(agent.RunOpts{})
				return &StepOutcome{}, err
			}}
			cfg := &config.Config{AgentCrashRetries: 1, AgentTimeout: time.Second, ReviewAgentTimeoutTightened: true}
			if !cfg.AgentTimeoutConfiguredFor(types.StepReview) || cfg.AgentTimeoutConfiguredFor(types.StepDocument) {
				t.Fatalf("a tightened review timeout must be configured for Review only")
			}
			executor := NewExecutor(database, p, cfg, ag, []Step{step}, nil)
			err = executor.Execute(context.Background(), run, repo, work)
			wantRetry := stepName != types.StepReview
			if (err == nil) != wantRetry || calls != 1+boolInt(wantRetry) {
				t.Fatalf("calls=%d err=%v, want retry=%t", calls, err, wantRetry)
			}
		})
	}
}

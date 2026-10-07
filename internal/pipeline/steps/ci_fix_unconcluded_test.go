package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Regression tests for a CI fix agent that stops part-way: an unfinished merge
// or a declared stop must never be committed or published, the agent's bytes
// stay where it left them, and an ordinary finished repair still publishes.
// They use only identifiers that already exist upstream so the same file runs
// against the unpatched base.

func unconcludedGit(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=a@x", "GIT_EDITOR=true")
	_ = cmd.Run()
}

// newUnconcludedFixture advances main past the reviewed feature head, on the
// same file when conflict is true, and wires a fake agent.
func newUnconcludedFixture(t *testing.T, conflict bool, act func(dir string), conclusion string) *ciRepairFixture {
	t.Helper()
	f := newCIRepairFixture(t, false, nil)
	gitCmd(t, f.dir, "checkout", "main")
	name := "main-only.txt"
	if conflict {
		name = "feature.txt"
	}
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte("main side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "advance main")
	gitCmd(t, f.dir, "checkout", "feature")
	f.sctx.Agent = &mockAgent{name: "native", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		act(opts.CWD)
		return &agent.Result{Output: json.RawMessage(conclusion)}, nil
	}}
	f.sctx.Ctx = context.Background()
	f.sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test","check_id":"github-check-run:42"}]}`
	return f
}

func (f *ciRepairFixture) repairRound(t *testing.T) *pipeline.StepOutcome {
	t.Helper()
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, CompletedAt: time.Now()}}}
	outcome, err := (&CIStep{}).repairFromFindings(f.sctx, host, &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("repair round error: %v\nlog:\n%s", err, f.log())
	}
	return outcome
}

func mergeHeadPresent(t *testing.T, dir string) bool {
	t.Helper()
	path := gitCmd(t, dir, "rev-parse", "--git-path", "MERGE_HEAD")
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	_, err := os.Stat(path)
	return err == nil
}

func leaveMerge(dir string) { unconcludedGit(dir, "merge", "--no-edit", "main") }

func TestCIRepair_UnfinishedMergeIsNeverPublished(t *testing.T) {
	// Today's agents report true with a prose refusal; the guard must not
	// depend on the conclusion at all.
	f := newUnconcludedFixture(t, true, leaveMerge, `{"summary":"admission refused; merge left unresolved","code_change_needed":true}`)
	outcome := f.repairRound(t)
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s; an unfinished merge was published", got)
	}
	if published := gitCmd(t, f.upstream, "show", "refs/heads/feature:feature.txt"); strings.Contains(published, "<<<<<<<") {
		t.Errorf("published content carries conflict markers:\n%s", published)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want the round parked for a decision", outcome)
	}
	if !mergeHeadPresent(t, f.dir) {
		t.Error("the agent's unfinished merge was not left in place")
	}
	if status := gitCmd(t, f.dir, "status", "--porcelain"); !strings.Contains(status, "AA feature.txt") {
		t.Errorf("conflicted bytes were not preserved; status:\n%s", status)
	}
}

func TestCIRepair_StoppedChangedHeadIsRetainedNotPublished(t *testing.T) {
	f := newUnconcludedFixture(t, false, leaveMerge, `{"summary":"post-merge admission refused","code_change_needed":true,"stopped":true}`)
	outcome := f.repairRound(t)
	local := f.localHead(t)
	if local == f.headSHA {
		t.Fatal("fixture error: the agent's merge commit is missing")
	}
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s; a stopped repair was published", got)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want the round parked for a decision", outcome)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA != nil && *run.LastPushedSHA == local {
		t.Error("a stopped repair was recorded as published")
	}
	if f.localHead(t) != local {
		t.Error("the agent's commit was not kept in the run worktree")
	}
}

func TestCIRepair_StoppedWithoutChangesParks(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(string) {}, `{"summary":"pre-merge admission refused","code_change_needed":true,"stopped":true}`)
	outcome := f.repairRound(t)
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s", got)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want a declared stop to park instead of resuming monitoring", outcome)
	}
}

func TestCIRepair_ResolvedMergeStillPublishes(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(dir string) {
		leaveMerge(dir)
		_ = os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("main side\nfeature\n"), 0o644)
		unconcludedGit(dir, "add", "-A")
		unconcludedGit(dir, "commit", "--no-edit")
	}, `{"summary":"merged main and resolved the conflict","code_change_needed":true}`)
	outcome := f.repairRound(t)
	local := f.localHead(t)
	if local == f.headSHA || f.remoteHead(t) != local {
		t.Errorf("remote = %s, local = %s; the finished repair was not published", f.remoteHead(t), local)
	}
	if published := gitCmd(t, f.upstream, "show", "refs/heads/feature:feature.txt"); strings.Contains(published, "<<<<<<<") {
		t.Errorf("published content carries conflict markers:\n%s", published)
	}
	if outcome != nil && outcome.NeedsApproval {
		t.Errorf("outcome = %#v, want monitoring to resume after a published repair", outcome)
	}
}

// The shared catch-all staging boundary serves CI repair commits, the
// Review/Test/Document/Lint fix commits, and Push's leftover commit.
func TestStagePipelineChanges_RefusesUnfinishedMerge(t *testing.T) {
	f := newUnconcludedFixture(t, true, leaveMerge, `{}`)
	leaveMerge(f.dir)
	if !mergeHeadPresent(t, f.dir) {
		t.Fatal("fixture error: no unfinished merge")
	}
	if err := stagePipelineChanges(f.sctx); err == nil {
		t.Error("staging succeeded over an unfinished merge")
	}
	if unmerged := gitCmd(t, f.dir, "diff", "--name-only", "--diff-filter=U"); unmerged != "feature.txt" {
		t.Errorf("unmerged paths after refusal = %q, want the index left untouched", unmerged)
	}
	if err := commitAgentFixes(f.sctx, types.StepReview, "apply review fix", ""); err == nil {
		t.Error("a Review fix commit concluded an unfinished merge")
	}
	if got := f.localHead(t); got != f.headSHA {
		t.Errorf("HEAD moved to %s", got)
	}
	if !mergeHeadPresent(t, f.dir) {
		t.Error("the unfinished merge was not left in place")
	}
}

func TestCIRepair_RefusalControlsSurviveInvalidSummary(t *testing.T) {
	long, _ := json.Marshal(strings.Repeat("界", 2000))
	for _, tc := range []struct {
		name, conclusion string
		dirty, unchanged bool
	}{
		{name: "stopped_long_summary", conclusion: `{"summary":` + string(long) + `,"code_change_needed":true,"stopped":true}`},
		{name: "no_change_long_summary", conclusion: `{"summary":` + string(long) + `,"code_change_needed":false}`},
		{name: "no_change_dirty", conclusion: `{"summary":` + string(long) + `,"code_change_needed":false}`, dirty: true},
		{name: "malformed_stopped", conclusion: `{"summary":"repair","code_change_needed":true,"stopped":"true"}`},
		{name: "malformed_change", conclusion: `{"summary":"repair","code_change_needed":"true"}`},
		{name: "malformed_json", conclusion: `{"summary":"repair","code_change_needed":true`},
		{name: "missing_change", conclusion: `{"summary":"repair"}`},
		{name: "no_change_committed", conclusion: `{"summary":"external failure","code_change_needed":false}`},
		{name: "valid_no_change_dirty", conclusion: `{"summary":"external failure","code_change_needed":false}`, dirty: true},
		{name: "unchanged_no_change", conclusion: `{"summary":"external failure","code_change_needed":false}`, unchanged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnconcludedFixture(t, false, func(dir string) {
				if tc.unchanged {
					return
				}
				if tc.dirty {
					if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("keep these bytes\n"), 0600); err != nil {
						t.Fatal(err)
					}
					return
				}
				leaveMerge(dir)
			}, tc.conclusion)
			outcome := f.repairRound(t)
			if f.remoteHead(t) != f.headSHA {
				t.Error("refused or invalid conclusion published a repair")
			}
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("outcome=%#v, want parked refusal", outcome)
			}
			if tc.dirty {
				if got, err := os.ReadFile(filepath.Join(f.dir, "dirty.txt")); err != nil || string(got) != "keep these bytes\n" {
					t.Fatalf("dirty bytes=%q, %v", got, err)
				}
				if f.localHead(t) != f.headSHA {
					t.Error("dirty work committed")
				}
			}
			if !tc.dirty && !tc.unchanged {
				run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.HeadSHA != f.localHead(t) {
					t.Error("local repair not retained in custody")
				}
			}
		})
	}
}

func TestCIRepair_ParserKeepsControlsOnSummaryError(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(map[string]any{"summary": strings.Repeat("界", 2000), "code_change_needed": false, "stopped": true})
	got, err := extractCIFixConclusion(&agent.Result{Output: raw})
	if err == nil || got.Stopped == nil || !*got.Stopped || got.CodeChangeNeeded == nil || *got.CodeChangeNeeded {
		t.Fatalf("controls=%+v, err=%v", got, err)
	}
}

func TestCIRepair_UnfinishedRebaseReconciliationAndReentry(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(dir string) {
		unconcludedGit(dir, "rebase", "main")
		_ = os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("preserve untracked\n"), 0600)
	}, `{"summary":"conflict remains","code_change_needed":true}`)
	if out := f.repairRound(t); out == nil || !out.NeedsApproval {
		t.Fatalf("outcome=%#v", out)
	}
	head := f.localHead(t)
	if head == f.headSHA || !rebaseInProgress(f.sctx.Ctx, f.dir) {
		t.Fatal("fixture has no divergent partial rebase")
	}
	index := gitCmd(t, f.dir, "ls-files", "--stage")
	bytes, err := os.ReadFile(filepath.Join(f.dir, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	step := &CIStep{}
	if resolved, err := step.ReconcileApprovalGate(f.sctx); err != nil || resolved {
		t.Errorf("reconcile=%v, %v; want parked", resolved, err)
	}
	f.sctx.Fixing = true
	f.sctx.Agent = &mockAgent{name: "must-not-launch", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Error("agent relaunched over unfinished operation")
		return nil, errors.New("unexpected launch")
	}}
	if out, err := step.Execute(f.sctx); err != nil || out == nil || !out.NeedsApproval {
		t.Errorf("reentry=%#v, %v", out, err)
	}
	if f.localHead(t) != head || gitCmd(t, f.dir, "ls-files", "--stage") != index {
		t.Error("partial HEAD/index changed")
	}
	after, err := os.ReadFile(filepath.Join(f.dir, "feature.txt"))
	if err != nil || string(after) != string(bytes) {
		t.Error("conflict bytes changed")
	}
	if f.remoteHead(t) != f.headSHA {
		t.Error("unfinished rebase published")
	}
}

type unconcludedExecutorStep struct {
	*CIStep
	f          *ciRepairFixture
	reconciled chan error
	initial    bool
}

func (s *unconcludedExecutorStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.f.sctx.Ctx = ctx.Ctx
	if s.initial {
		return s.CIStep.Execute(s.f.sctx)
	}
	return s.CIStep.repairFromFindings(s.f.sctx, &completionSnapshotHost{checks: []scm.Check{{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, CompletedAt: time.Now()}}}, &scm.PR{Number: "42"})
}
func (s *unconcludedExecutorStep) ReconcileApprovalGate(ctx *pipeline.StepContext) (bool, error) {
	resolved, err := s.CIStep.ReconcileApprovalGate(ctx)
	select {
	case s.reconciled <- err:
	default:
	}
	return resolved, err
}
func TestCIRepair_ExecutorKeepsUnfinishedRebaseParked(t *testing.T) {
	for _, mode := range []struct{ initial, recovered bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		initial, recovered := mode.initial, mode.recovered
		name := "live"
		if recovered {
			name = "recovered"
		}
		t.Run(fmt.Sprintf("%s/initial=%t", name, initial), func(t *testing.T) {
			f := newUnconcludedFixture(t, true, func(dir string) { unconcludedGit(dir, "rebase", "main") }, `{"summary":"conflict remains","code_change_needed":true}`)
			step := &unconcludedExecutorStep{CIStep: &CIStep{}, f: f, reconciled: make(chan error, 1), initial: initial}
			if initial {
				unconcludedGit(f.dir, "rebase", "main")
				f.sctx.PreviousFindings = ""
			}
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if recovered {
				if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				if out, err := step.Execute(f.sctx); err != nil || out == nil || !out.NeedsApproval {
					t.Fatalf("repair did not park: %#v %v", out, err)
				} else {
					sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.sctx.DB.StartStep(sr.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.sctx.DB.InsertStepRound(sr.ID, 1, "initial", &out.Findings, nil, 1); err != nil {
						t.Fatal(err)
					}
					if err := f.sctx.DB.ParkStepForApproval(f.sctx.Run.ID, sr.ID, types.StepStatusAwaitingApproval, out.ExitCode, 1, &out.Findings); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				f.sctx.Run, err = f.sctx.DB.GetRun(f.sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			parked := make(chan struct{}, 1)
			executor := pipeline.NewExecutor(f.sctx.DB, p, f.sctx.Config, f.sctx.Agent, []pipeline.Step{step}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					parked <- struct{}{}
				}
			})
			executor.SetGateReconcileTimings(time.Hour, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if recovered {
					done <- executor.Resume(ctx, f.sctx.Run, f.sctx.Repo, f.dir)
				} else {
					done <- executor.Execute(ctx, f.sctx.Run, f.sctx.Repo, f.dir)
				}
			}()
			select {
			case <-parked:
			case <-time.After(10 * time.Second):
				t.Fatal("executor did not register approval gate")
			}
			select {
			case err := <-step.reconciled:
				if err != nil {
					t.Errorf("actual executor reconciliation: %v", err)
				}
			case err := <-done:
				t.Fatalf("executor stopped before reconciliation: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("executor did not reconcile")
			}
			if err := executor.Respond(types.StepCI, types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "use fix") {
				t.Errorf("ordinary approval must refuse unfinished work and explain retry: %v", err)
			}
			select {
			case err := <-done:
				t.Fatalf("park unexpectedly ended: %v", err)
			default:
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run.AwaitingAgentSince == nil {
				t.Errorf("run=%+v err=%v", run, err)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("executor did not cancel")
			}
			if !rebaseInProgress(context.Background(), f.dir) || f.remoteHead(t) != f.headSHA {
				t.Error("executor lost or published partial rebase")
			}
		})
	}
}

func TestCIRepair_CleanDivergentHeadStillFailsContinuity(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(string) {}, `{}`)
	gitCmd(t, f.dir, "checkout", "--detach", "main")
	if resolved, err := (&CIStep{}).ReconcileApprovalGate(f.sctx); resolved || !errors.Is(err, pipeline.ErrFatalGateReconciliation) {
		t.Fatalf("clean divergent reconciliation=%v, %v", resolved, err)
	}
	if out, err := (&CIStep{}).Execute(f.sctx); err == nil {
		t.Fatalf("clean divergent execution=%#v, want continuity refusal", out)
	}
	if f.remoteHead(t) != f.headSHA {
		t.Error("divergent head published")
	}
}

func TestCIRepair_UnfinishedReconciliationUsesStepEnv(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(string) {}, `{}`)
	leaveMerge(f.dir)
	if !mergeHeadPresent(t, f.dir) {
		t.Fatal("fixture error: no unfinished merge")
	}
	// The step still has its working Git PATH; the ambient process does not.
	t.Setenv("PATH", t.TempDir())
	if resolved, err := (&CIStep{}).ReconcileApprovalGate(f.sctx); resolved || err != nil {
		t.Fatalf("unfinished reconciliation = %v, %v; want parked without error", resolved, err)
	}
}

func ciConclusionAdapter(t *testing.T, conclusion string) agent.Agent {
	t.Helper()
	dir := t.TempDir()
	event, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": conclusion}})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output.jsonl")
	if err := os.WriteFile(output, append(event, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	name, script := "codex", "#!/bin/sh\ncat '"+strings.ReplaceAll(output, "'", "'\"'\"'")+"'\n"
	if runtime.GOOS == "windows" {
		name, script = "codex.cmd", "@echo off\r\ntype \""+output+"\"\r\n"
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	adapter, err := agent.New(types.AgentCodex, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapter.Close() })
	return adapter
}

func TestCIRepair_CodexOptionalStopPublishesAcceptedMerge(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"omitted", ""},
		{"null", `,"stopped":null`},
		{"false", `,"stopped":false`},
		{"true", `,"stopped":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field := tc.field
			adapter := ciConclusionAdapter(t, `{"summary":"accepted merge","code_change_needed":true`+field+`}`)
			f := newUnconcludedFixture(t, false, leaveMerge, `{}`)
			f.sctx.Agent = &mockAgent{name: "codex", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				leaveMerge(opts.CWD)
				return adapter.Run(ctx, opts)
			}}
			out := f.repairRound(t)
			parents := strings.Fields(gitCmd(t, f.dir, "rev-list", "--parents", "-n", "1", "HEAD"))
			if len(parents) != 3 {
				t.Fatalf("accepted fixture is not a merge: %v", parents)
			}
			if strings.Contains(field, "true") {
				if out == nil || !out.NeedsApproval || f.remoteHead(t) != f.headSHA {
					t.Fatalf("stop outcome=%#v remote=%s", out, f.remoteHead(t))
				}
			} else if f.remoteHead(t) != f.localHead(t) || f.remoteHead(t) == f.headSHA || (out != nil && out.NeedsApproval) {
				t.Fatalf("accepted Codex repair parked/unpublished: outcome=%#v log=%s", out, f.log())
			}
		})
	}
}

func TestCIRepair_CodexStructuredRejectionParks(t *testing.T) {
	long, _ := json.Marshal(strings.Repeat("界", 5000))
	conclusions := []string{
		`{"summary":` + string(long) + `,"code_change_needed":true,"stopped":true}`,
		`{"summary":42,"code_change_needed":true,"stopped":true}`,
		`{"summary":"partial","code_change_needed":null,"stopped":true}`,
		`{"summary":"partial","stopped":true}`,
		`{"summary":"partial","code_change_needed":"true"}`,
		`{"summary":"partial","code_change_needed":true,"stopped":"true"}`,
		`{"summary":"partial","code_change_needed":true`,
	}
	for i, conclusion := range conclusions {
		for _, mutation := range []string{"dirty", "commit", "merge", "none"} {
			t.Run(fmt.Sprintf("%d/%s", i, mutation), func(t *testing.T) {
				adapter := ciConclusionAdapter(t, conclusion)
				f := newUnconcludedFixture(t, mutation == "merge", func(string) {}, `{}`)
				var rejection error
				var head, index, bytes, rawIndex string
				f.sctx.Agent = &mockAgent{name: "codex", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					switch mutation {
					case "dirty":
						if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("staged\n"), 0600); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, opts.CWD, "add", "feature.txt")
						if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("dirty\x00bytes\n"), 0600); err != nil {
							t.Fatal(err)
						}
					case "commit", "merge":
						leaveMerge(opts.CWD)
					}
					head = f.localHead(t)
					indexPath := gitCmd(t, f.dir, "rev-parse", "--git-path", "index")
					if !filepath.IsAbs(indexPath) {
						indexPath = filepath.Join(f.dir, indexPath)
					}
					indexRaw, indexErr := os.ReadFile(indexPath)
					if indexErr != nil {
						t.Fatal(indexErr)
					}
					rawIndex = string(indexRaw)
					index = gitCmd(t, f.dir, "ls-files", "--stage")
					raw, err := os.ReadFile(filepath.Join(f.dir, "feature.txt"))
					if err != nil {
						t.Fatal(err)
					}
					bytes = string(raw)
					result, err := adapter.Run(ctx, opts)
					rejection = err
					return result, err
				}}
				out := f.repairRound(t)
				if i == 0 {
					if rejection != nil {
						t.Fatalf("oversized summary unexpectedly rejected by adapter: %v", rejection)
					}
				} else if !agent.IsStructuredOutputRejected(rejection) {
					t.Fatalf("not an adapter rejection: %v; log: %s", rejection, f.log())
				}
				if out == nil || !out.NeedsApproval || out.AutoFixable {
					t.Fatalf("schema rejection retried instead of parked: %#v", out)
				}
				if f.remoteHead(t) != f.headSHA || f.localHead(t) != head || gitCmd(t, f.dir, "ls-files", "--stage") != index {
					t.Fatal("rejected repair published or mutated HEAD/index")
				}
				raw, err := os.ReadFile(filepath.Join(f.dir, "feature.txt"))
				if err != nil || string(raw) != bytes {
					t.Fatal("rejected repair bytes changed")
				}
				indexPath := gitCmd(t, f.dir, "rev-parse", "--git-path", "index")
				if !filepath.IsAbs(indexPath) {
					indexPath = filepath.Join(f.dir, indexPath)
				}
				indexRaw, indexErr := os.ReadFile(indexPath)
				if indexErr != nil || string(indexRaw) != rawIndex {
					t.Fatal("raw index changed after rejection")
				}
				if mutation == "merge" && !mergeHeadPresent(t, f.dir) {
					t.Fatal("unfinished merge lost")
				}
				if mutation == "commit" {
					run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
					if err != nil || run.HeadSHA != head || run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA {
						t.Fatalf("unpublished custody: %+v %v", run, err)
					}
				}
			})
		}
	}
}

func TestCIRepair_UnrelatedAgentErrorStillUsesOrdinaryPolicy(t *testing.T) {
	f := newUnconcludedFixture(t, false, func(string) {}, `{}`)
	f.sctx.Agent = &mockAgent{name: "failed", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("transport failed")
	}}
	if out := f.repairRound(t); out != nil {
		t.Fatalf("unrelated error parked: %#v", out)
	}
	if f.remoteHead(t) != f.headSHA || f.localHead(t) != f.headSHA {
		t.Fatal("unrelated error moved HEAD")
	}
}

func TestCIRepair_StoppedDirtyWithRevalidationPolicy(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		t.Run(fmt.Sprint(revalidate), func(t *testing.T) {
			f := newUnconcludedFixture(t, false, func(dir string) {
				if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("retained\x00bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}, `{"summary":"stopped","code_change_needed":true,"stopped":true}`)
			f.sctx.Config.CI.RevalidateRepairs = revalidate
			out := f.repairRound(t)
			if out == nil || !out.NeedsApproval || out.RestartFrom != "" || f.remoteHead(t) != f.headSHA || f.localHead(t) != f.headSHA {
				t.Fatalf("stopped policy outcome=%#v", out)
			}
			raw, err := os.ReadFile(filepath.Join(f.dir, "dirty.txt"))
			if err != nil || string(raw) != "retained\x00bytes" {
				t.Fatal("stopped bytes lost")
			}
		})
	}
}

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
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_ProtectedPathRetryUsesPersistedRepair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		rebase     bool
		revalidate bool
		addOnly    bool
		unverified bool
	}{
		{name: "added_finding_only_publish", addOnly: true},
		{name: "added_finding_only_revalidate", addOnly: true, revalidate: true},
		{name: "rebased_conflict", rebase: true},
		{name: "unverified_rewrite_stays_refused", rebase: true, unverified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCIRepairFixture(t, tc.revalidate, nil)
			if tc.rebase {
				gitCmd(t, f.dir, "checkout", "main")
				if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("base change\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", "feature.txt")
				gitCmd(t, f.dir, "commit", "-m", "advance conflicting base")
				gitCmd(t, f.dir, "push", "origin", "main")
				gitCmd(t, f.dir, "checkout", "feature")
				f.sctx.Env = fakeCIGHMergeable(t, "OPEN", `[]`, "CONFLICTING")
			}
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if tc.rebase {
					if _, err := stepGitRun(f.sctx, "rebase", "main"); err == nil {
						t.Error("fixture rebase must conflict")
					}
					if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("base change\nresolved feature\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, f.dir, "add", "feature.txt")
					gitCmd(t, f.dir, "-c", "core.editor=true", "rebase", "--continue")
				}
				if err := os.WriteFile(filepath.Join(f.dir, "package.lock"), []byte("staged lock\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", "package.lock")
				for file, content := range map[string]string{"package.lock": "refused lock\n", "fix.go": "retained repair\n"} {
					if err := os.WriteFile(filepath.Join(f.dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"repair CI","code_change_needed":true}`)}, nil
			}}
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
			}
			if got := gitCmd(t, f.dir, "show", ":package.lock"); got != "staged lock" {
				t.Fatalf("refusal changed index: %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(f.dir, "package.lock")); err != nil || string(got) != "refused lock\n" {
				t.Fatalf("refusal changed working file: %q, %v", got, err)
			}
			for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepPush} {
				sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.sctx.DB.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
			}
			persistCIRefusal(t, f, outcome)
			gitCmd(t, f.dir, "restore", "--source=HEAD", "--staged", "--worktree", "--", "package.lock")
			if tc.unverified {
				gitCmd(t, f.dir, "update-ref", "HEAD", "main")
			}
			f.sctx.Config.Commands.Test = "git cat-file -e HEAD:fix.go"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			green := fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			ci := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { cancel(); return ctx.Err() }}
			steps := []pipeline.Step{&ReviewStep{}, &TestStep{}, &PushStep{}, reconcileEnvStep{step: ci, env: green}}
			reviews := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				reviews++
				output, err := json.Marshal(cleanReviewFindings())
				return &agent.Result{Output: output}, err
			}}
			parked := make(chan struct{}, 1)
			gates := 0
			executor := pipeline.NewExecutor(f.sctx.DB, paths.WithRoot(t.TempDir()), f.sctx.Config, ag, steps, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
					gates++
					if gates == 1 {
						parked <- struct{}{}
					} else {
						cancel()
					}
				}
			})
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- executor.Resume(ctx, run, f.sctx.Repo, f.dir) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Error("executor did not stop")
				}
			})
			select {
			case <-parked:
			case err := <-done:
				done <- err
				t.Fatalf("could not resume refusal: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("refusal did not resume")
			}
			refused, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(refused.Items) != 1 {
				t.Fatalf("invalid refusal: %+v, %v", refused, err)
			}
			selected := []string{refused.Items[0].ID}
			var added []types.Finding
			if tc.addOnly {
				selected = []string{}
				added = []types.Finding{{ID: "user-1", Severity: "info", Description: "publish the retained repair", Action: types.ActionAutoFix}}
			}
			if err := executor.RespondWithOverrides(types.StepCI, types.ActionFix, selected, nil, added); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				done <- err
			case <-time.After(15 * time.Second):
				t.Fatal("explicit retry did not finish")
			}
			local, remote := f.localHead(t), f.remoteHead(t)
			if tc.unverified {
				if gates != 2 || reviews != 0 || remote != f.headSHA || !strings.Contains(gitStatusPorcelain(t, f.dir), "fix.go") {
					t.Fatalf("unverified head escaped refusal: gates=%d reviews=%d remote=%s dirty=%q error=%v", gates, reviews, remote, gitStatusPorcelain(t, f.dir), err)
				}
				t.Logf("unverified retry: gates=%d reviews=%d remote=%s status=%q", gates, reviews, remote, gitStatusPorcelain(t, f.dir))
				return
			}
			if gates != 1 || remote == f.headSHA || remote != local || gitStatusPorcelain(t, f.dir) != "" {
				t.Fatalf("retry stranded retained repair: gates=%d local=%s remote=%s dirty=%q error=%v", gates, local, remote, gitStatusPorcelain(t, f.dir), err)
			}
			if got := gitCmd(t, f.upstream, "show", "refs/heads/feature:fix.go"); got != "retained repair" {
				t.Fatalf("publication lost retained repair: %q", got)
			}
			wantReviews := 0
			if tc.rebase || tc.revalidate {
				wantReviews = 1
			}
			if reviews != wantReviews {
				t.Fatalf("review executions = %d, want %d", reviews, wantReviews)
			}
			results, err := f.sctx.DB.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wantReviews == 1 {
				for _, sr := range results[:3] {
					if sr.Status != types.StepStatusCompleted || sr.StartedAt == nil {
						t.Fatalf("repair skipped required %s: %+v", sr.StepName, sr)
					}
					t.Logf("persisted revalidation: step=%s status=%s started_at=%d", sr.StepName, sr.Status, *sr.StartedAt)
				}
			}
			t.Logf("retained CI repair publication: before=%s local=%s remote=%s remote:fix.go=%q status=%q", f.headSHA, local, remote, gitCmd(t, f.upstream, "show", "refs/heads/feature:fix.go"), gitStatusPorcelain(t, f.dir))
		})
	}
}

func persistCIRefusal(t *testing.T, f *ciRepairFixture, outcome *pipeline.StepOutcome) {
	t.Helper()
	if f.sctx.StepResultID == "" {
		sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
		if err != nil {
			t.Fatal(err)
		}
		f.sctx.StepResultID = sr.ID
	}
	if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.StartStep(f.sctx.StepResultID); err != nil {
		t.Fatal(err)
	}
	rounds, err := f.sctx.DB.GetRoundsByStep(f.sctx.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sctx.DB.InsertStepRound(f.sctx.StepResultID, len(rounds)+1, "initial", &outcome.Findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.ParkStepForApproval(f.sctx.Run.ID, f.sctx.StepResultID, types.StepStatusAwaitingApproval, 1, &outcome.Findings); err != nil {
		t.Fatal(err)
	}
}

func TestCIStep_ProtectedPathRetryFinishesRetainedRepairWithGreenChecks(t *testing.T) {
	t.Parallel()
	for _, revalidate := range []bool{false, true} {
		name := "publish"
		if revalidate {
			name = "revalidate"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			f := newCIRepairFixture(t, revalidate, func(dir string) {
				calls++
				for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "retained repair\n"} {
					if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			})
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
			}
			persistCIRefusal(t, f, outcome)
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			outcome, err = f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) || calls != 1 || f.localHead(t) != f.headSHA {
				t.Fatalf("unresolved retry bypassed refusal or reran fixer: %+v, %v calls=%d", outcome, err, calls)
			}
			if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
				t.Fatal(err)
			}
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			outcome, err = f.run(t)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("retry: %+v, %v\n%s", outcome, err, f.log())
			}
			if f.localHead(t) == f.headSHA {
				t.Fatalf("green checks bypassed retained repair: remote=%s dirty=%q\n%s", f.remoteHead(t), gitStatusPorcelain(t, f.dir), f.log())
			}
			if calls != 1 {
				t.Fatalf("retry ran another fixer over retained work: %d calls", calls)
			}
			if got := gitCmd(t, f.dir, "show", "HEAD:fix.go"); got != "retained repair" {
				t.Fatalf("commit lost retained repair: %q", got)
			}
			if revalidate {
				if outcome == nil || outcome.RestartFrom != types.StepReview || f.remoteHead(t) != f.headSHA {
					t.Fatalf("retry skipped required pipeline revalidation: %+v remote=%s", outcome, f.remoteHead(t))
				}
				if strings.Contains(f.log(), ciChecksPassedMsg) {
					t.Fatal("reported checks passed before revalidation/publication")
				}
			} else if f.remoteHead(t) != f.localHead(t) || !strings.Contains(f.log(), ciChecksPassedMsg) {
				t.Fatalf("retry did not publish before monitoring: local=%s remote=%s\n%s", f.localHead(t), f.remoteHead(t), f.log())
			}
		})
	}
}

func TestCIStep_ProtectedPathRetryPublicationFailureKeepsRefusal(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, func(dir string) {
		for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "retained repair\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	f.sctx.Config.ProtectedPaths = []string{"*.lock"}
	outcome, err := f.run(t)
	if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
		t.Fatalf("repair did not refuse: %+v, %v", outcome, err)
	}
	persistCIRefusal(t, f, outcome)
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = outcome.Findings
	if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
		t.Fatal(err)
	}
	f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
	hooks := t.TempDir()
	gitCmd(t, f.upstream, "config", "core.hooksPath", hooks)
	rejectPush := filepath.Join(hooks, "pre-receive")
	if err := os.WriteFile(rejectPush, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outcome, err = f.run(t)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
		t.Fatalf("unfinished publication lost refusal: %+v, %v\n%s", outcome, err, f.log())
	}
	persistCIRefusal(t, f, outcome)
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].File != "package.lock" || !strings.Contains(findings.Items[0].Description, `rule "*.lock"`) {
		t.Fatalf("retry lost original path or rule: %+v, %v", findings, err)
	}
	if strings.Contains(f.log(), ciChecksPassedMsg) || f.remoteHead(t) != f.headSHA {
		t.Fatal("unfinished publication advanced remote or reported checks passed")
	}
	if got, err := os.ReadFile(filepath.Join(f.dir, "fix.go")); err != nil || string(got) != "retained repair\n" {
		t.Fatalf("lost retained work: %q, %v", got, err)
	}
	f.sctx.PreviousFindings = outcome.Findings
	if err := os.Remove(rejectPush); err != nil {
		t.Fatal(err)
	}
	outcome, err = f.run(t)
	if !errors.Is(err, context.Canceled) || f.remoteHead(t) == f.headSHA || f.remoteHead(t) != f.localHead(t) {
		t.Fatalf("second explicit retry did not finish publication: %+v, %v\n%s", outcome, err, f.log())
	}
}

func TestCIStep_ProtectedPathRefusalStopsAutomaticAndManualRepair(t *testing.T) {
	t.Parallel()
	for _, manual := range []bool{false, true} {
		name := "automatic"
		if manual {
			name = "manual"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			invocations := 0
			var indexBefore string
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				invocations++
				if err := os.WriteFile(filepath.Join(dir, "package.lock"), []byte("staged lock\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "package.lock")
				indexBefore = gitCmd(t, dir, "diff", "--cached")
				for file, content := range map[string]string{"package.lock": "rejected edit\n", "fix.txt": "CI repair\n"} {
					if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"repair checks","code_change_needed":true}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.ProtectedPaths = []string{"*.lock"}
			sctx.Config.AutoFix.CI = 3
			sctx.Config.CITimeout = time.Minute
			sctx.Fixing = manual
			polls := 0
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				polls++
				return nil
			}}
			outcome, err := step.Execute(sctx)
			if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("refusal must park for an operator: outcome=%+v err=%v", outcome, err)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("refusal findings=%+v err=%v", findings, err)
			}
			finding := findings.Items[0]
			if finding.File != "package.lock" || finding.Action != types.ActionAskUser || !strings.Contains(finding.Description, `rule "*.lock"`) {
				t.Errorf("refusal lost the path, rule, or decision: %+v", finding)
			}
			if invocations != 1 || polls != 0 {
				t.Errorf("refusal retried: invocations=%d polls=%d", invocations, polls)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Errorf("refusal committed: HEAD=%s want %s", got, headSHA)
			}
			if got := gitCmd(t, dir, "diff", "--cached"); got != indexBefore {
				t.Errorf("refusal changed the index: %q", got)
			}
			for file, want := range map[string]string{"package.lock": "rejected edit\n", "fix.txt": "CI repair\n"} {
				if got, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(got) != want {
					t.Errorf("refusal discarded %s: %q err=%v", file, got, err)
				}
			}
		})
	}
}

func TestTestStep_FixMode_ProtectedPathDoesNotReachCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	const ledger = "generated-ledger.json"
	if err := os.WriteFile(filepath.Join(dir, ledger), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ledger)
	gitCmd(t, dir, "commit", "-m", "add tool-owned ledger")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		for file, content := range map[string]string{ledger: "unrelated agent edit\n", "fix.txt": "test repair\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"repair test failure"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	repo, err := config.LoadRepoFromBytes([]byte("commands:\n  test: exit 0\nprotected_paths:\n  - generated-ledger.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	sctx.Config = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(repo, repo, false))
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"test-1","severity":"error","file":"fix.txt","description":"test failed"}]}`
	_, err = (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), ledger) {
		t.Errorf("expected a surfaced protected-path error naming %s, got %v", ledger, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Errorf("fix committed an out-of-scope edit: HEAD = %s, want %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("refusal staged changes: %q", got)
	}
	if got := gitStatusPorcelain(t, dir); !strings.Contains(got, ledger) || !strings.Contains(got, "fix.txt") {
		t.Errorf("refusal must preserve both edits for inspection, status = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, ledger)); err != nil || string(got) != "unrelated agent edit\n" {
		t.Errorf("protected edit was discarded: content = %q, err = %v", got, err)
	}
	t.Logf("Test auto-fix refusal: %v\nHEAD=%s\nindex paths=%q\nworktree status:\n%s", err, gitCmd(t, dir, "rev-parse", "HEAD"), gitCmd(t, dir, "diff", "--cached", "--name-only"), gitStatusPorcelain(t, dir))
}

func TestProtectedPaths_AllAutomaticCommitPathsRefuseWithoutMutation(t *testing.T) {
	t.Parallel()
	for _, caller := range []struct {
		name string
		run  func(*pipeline.StepContext) error
	}{
		{"fix", func(sctx *pipeline.StepContext) error {
			return commitAgentFixes(sctx, types.StepDocument, "update docs", "")
		}},
		{"push", func(sctx *pipeline.StepContext) error {
			_, err := (&PushStep{}).Execute(sctx)
			return err
		}},
		{"ci", func(sctx *pipeline.StepContext) error {
			_, err := (&CIStep{}).commitRepair(sctx, "repair checks")
			return err
		}},
	} {
		t.Run(caller.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.ProtectedPaths = []string{"feature.txt"}
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("unrelated residue"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "feature.txt")
			before := gitCmd(t, dir, "diff", "--cached")
			err := caller.run(sctx)
			if err == nil || !strings.Contains(err.Error(), "feature.txt") {
				t.Fatalf("expected protected-path refusal, got %v", err)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Errorf("HEAD advanced from %s to %s", headSHA, got)
			}
			if got := gitCmd(t, dir, "diff", "--cached"); got != before {
				t.Errorf("refusal altered the index: %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "feature.txt")); err != nil || string(got) != "unrelated residue" {
				t.Errorf("refusal altered the worktree: %q, %v", got, err)
			}
		})
	}
}

func TestProtectedPaths_Staging(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, pattern, edit, file string
		blocked                   bool
	}{
		{"unstaged", "feature.txt", "write", "feature.txt", true},
		{"deleted", "feature.txt", "delete", "feature.txt", true},
		{"rename_source", "feature.txt", "rename", "renamed.txt", true},
		{"rename_destination", "renamed.txt", "rename", "renamed.txt", true},
		{"new_directory", "*.lock", "write", "new/nested/package.lock", true},
		{"subtree", "new/**", "write", "new/nested/package.lock", true},
		{"literal_path", "new/nested/package.lock", "write", "new/nested/package.lock", true},
		{"spaces_and_unicode", "*.lock", "write", "new/ ledger ü.lock", true},
		{"glob_does_not_cross_slash", "new/*.lock", "write", "new/nested/package.lock", false},
		{"clean_protected_file", "feature.txt", "write", "fix.txt", false},
		{"default_opt_out", "", "write", "feature.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
			if tc.pattern != "" {
				sctx.Config.ProtectedPaths = []string{tc.pattern}
			}
			file := filepath.Join(dir, filepath.FromSlash(tc.file))
			switch tc.edit {
			case "write":
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("agent edit"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "rename":
				gitCmd(t, dir, "mv", "feature.txt", tc.file)
			}
			before := gitCmd(t, dir, "diff", "--cached")
			err := stagePipelineChanges(sctx)
			if tc.blocked {
				if err == nil || !strings.Contains(err.Error(), "protected") {
					t.Fatalf("expected protected-path error, got %v", err)
				}
				if got := gitCmd(t, dir, "diff", "--cached"); got != before {
					t.Errorf("refusal changed index: %q", got)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); !strings.Contains(got, tc.file) {
					t.Errorf("allowed edit not staged: %q", got)
				}
			}
		})
	}
}

func TestProtectedPaths_UnreadableStatusFailsClosed(t *testing.T) {
	t.Parallel()
	sctx := newTestContext(t, &mockAgent{}, t.TempDir(), "", "", config.Commands{})
	sctx.Config.ProtectedPaths = []string{"*.lock"}
	if err := stagePipelineChanges(sctx); err == nil || !strings.Contains(err.Error(), "check protected_paths") {
		t.Fatalf("unreadable git status did not fail closed: %v", err)
	}
}

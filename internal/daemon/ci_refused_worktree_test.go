package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const ciRefusedFindings = `{"summary":"CI repair not concluded","findings":[{"id":"ci-1","severity":"error","description":"failed","action":"ask-user","category":"ci-check","check":"test"}]}`

type ciRefusedCleanupStep struct {
	*steps.CIStep
	env      []string
	findings string
}

func (s ciRefusedCleanupStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	findings := s.findings
	if findings == "" {
		findings = ciRefusedFindings
	}
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: findings}, nil
}
func (s ciRefusedCleanupStep) ReconcileApprovalGate(ctx *pipeline.StepContext) (bool, error) {
	ctx.Env = s.env
	return s.CIStep.ReconcileApprovalGate(ctx)
}

func refusedDirtyWorktree(t *testing.T, dir, mutation string) {
	t.Helper()
	for name, raw := range map[string]string{"test.txt": "unstaged tracked\x00bytes\n", "staged.txt": "staged bytes\n", "untracked.txt": "untracked\x00bytes\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "add", "staged.txt")
	if mutation == "commit" {
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "unpublished refused repair")
	} else if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("unstaged over staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCIRefusedWorkSurvivesTerminalExecutorAndCleanup(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, state := range []string{"CLOSED", "MERGED"} {
			for _, route := range []string{"immediate", "startup", "age", "count"} {
				for _, mutation := range []string{"dirty", "commit", "clean", "marker-cleared"} {
					mode := "live"
					if recovered {
						mode = "recovered"
					}
					t.Run(mode+"/"+state+"/"+route+"/"+mutation, func(t *testing.T) {
						placement := "default"
						if route == "startup" {
							placement = "recorded"
						}
						operation := "clean"
						if mutation == "marker-cleared" {
							operation = "merge"
						}
						p, database, repo, run, dir := unfinishedCleanupFixture(t, operation, placement)
						findings := ciRefusedFindings
						if mutation == "marker-cleared" {
							out, err := (&steps.CIStep{}).Execute(&pipeline.StepContext{Ctx: context.Background(), DB: database, Run: run, WorkDir: dir, Log: func(string) {}})
							if err != nil || out == nil || !out.NeedsApproval {
								t.Fatalf("initial unfinished gate=%#v %v", out, err)
							}
							parsed, err := types.ParseFindingsJSON(out.Findings)
							if err != nil || len(parsed.Items) != 1 || parsed.Items[0].ID != pipeline.CIIncompleteWorkFindingID {
								t.Fatalf("initial gate omitted refusal: %+v %v", parsed, err)
							}
							findings = out.Findings
							marker := gitOutput(t, dir, "rev-parse", "--git-path", "MERGE_HEAD")
							if !filepath.IsAbs(marker) {
								marker = filepath.Join(dir, marker)
							}
							if err := os.Remove(marker); err != nil {
								t.Fatal(err)
							}
							gitCmd(t, dir, "add", "test.txt")
						}
						published := run.HeadSHA
						if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: "fixture", Ref: "refs/heads/feature"}); err != nil {
							t.Fatal(err)
						}
						if mutation != "clean" && mutation != "marker-cleared" {
							refusedDirtyWorktree(t, dir, mutation)
							if mutation == "commit" {
								if err := database.UpdateRunHeadSHA(run.ID, gitOutput(t, dir, "rev-parse", "HEAD")); err != nil {
									t.Fatal(err)
								}
							}
						}
						var before map[string]string
						if mutation != "clean" {
							before = unfinishedWorktreeSnapshot(t, dir)
						}
						repo.UpstreamURL = "https://github.com/test/repo"
						if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
							t.Fatal(err)
						}
						if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
							t.Fatal(err)
						}
						if recovered {
							sr, err := database.InsertStepResult(run.ID, types.StepCI)
							if err != nil {
								t.Fatal(err)
							}
							if err := database.StartStep(sr.ID); err != nil {
								t.Fatal(err)
							}
							if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
								t.Fatal(err)
							}
							if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
								t.Fatal(err)
							}
						}
						run, err := database.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						ghDir, _ := writeMockGHState(t, t.TempDir(), state)
						t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
						env := os.Environ()
						step := ciRefusedCleanupStep{CIStep: &steps.CIStep{}, env: env, findings: findings}
						executor := pipeline.NewExecutor(database, p, &config.Config{}, nil, []pipeline.Step{step}, nil)
						executor.SetGateReconcileTimings(time.Hour, config.DefaultGateReconcileTimeout)
						ctx, cancel := context.WithTimeout(context.Background(), 2*config.DefaultGateReconcileTimeout)
						defer cancel()
						if recovered {
							err = executor.Resume(ctx, run, repo, dir)
						} else {
							err = executor.Execute(ctx, run, repo, dir)
						}
						if err != nil {
							t.Fatalf("terminal CI executor: %v", err)
						}
						run, err = database.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						if run.Status != types.RunCompleted || run.PRState == nil || (*run.PRState != "closed" && *run.PRState != "merged") || run.AwaitingAgentSince != nil {
							t.Fatalf("CI reconciliation not completed: %+v", run)
						}
						switch route {
						case "immediate":
							NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), dir, "fixture")
						case "startup":
							cleanupOrphanWorktrees(database, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: dir}})
						case "age":
							reapWorktrees(database, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
						case "count":
							newer := p.WorktreeDir(repo.ID, "newer-orphan")
							if err := os.MkdirAll(newer, 0755); err != nil {
								t.Fatal(err)
							}
							old := time.Now().Add(-time.Hour)
							if err := os.Chtimes(dir, old, old); err != nil {
								t.Fatal(err)
							}
							reapWorktrees(database, p, worktreeReapPolicy{MaxRuns: 1}, time.Now().Add(time.Hour))
						}
						_, err = os.Stat(dir)
						if mutation == "clean" {
							if !os.IsNotExist(err) {
								t.Fatalf("clean work retained: %v", err)
							}
							return
						}
						if err != nil {
							t.Fatalf("refused work deleted after terminal CI: %v", err)
						}
						if after := unfinishedWorktreeSnapshot(t, dir); !reflect.DeepEqual(before, after) {
							t.Fatal("terminal reconciliation/cleanup changed HEAD/raw index/bytes")
						}
						run, err = database.GetRun(run.ID)
						if err != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != published {
							t.Fatalf("unpublished custody changed: %+v %v", run, err)
						}
					})
				}
			}
		}
	}
}

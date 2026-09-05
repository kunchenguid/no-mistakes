package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type protectedPathPushRetryStep struct {
	edited bool
}

func TestProtectedPathRefusalCancellationCleanup(t *testing.T) {
	for _, action := range []string{"new_push", "cancel_run", "abort_response"} {
		t.Run(action, func(t *testing.T) {
			p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
				return []pipeline.Step{protectedPathCommitStep{step: &steps.PushStep{}}}
			})
			repo, head := setupTestGitRepo(t, p, database, "protected-cancellation")
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.PushReceivedResult
			push := &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: head}
			if err := client.Call(ipc.MethodPushReceived, push, &result); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				run, err := database.GetRun(result.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if run.AwaitingAgentSince != nil {
					break
				}
				if run.Status.Terminal() || time.Now().After(deadline) {
					t.Fatalf("refusal did not park: %+v", run)
				}
				time.Sleep(10 * time.Millisecond)
			}
			switch action {
			case "new_push":
				var replacement ipc.PushReceivedResult
				if err := client.Call(ipc.MethodPushReceived, push, &replacement); err != nil {
					t.Fatal(err)
				}
				if replacement.RunID == result.RunID {
					t.Fatal("new push did not supersede the parked run")
				}
			case "cancel_run":
				if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: result.RunID}, nil); err != nil {
					t.Fatal(err)
				}
			case "abort_response":
				if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: result.RunID, Step: types.StepPush, Action: types.ActionAbort}, nil); err != nil {
					t.Fatal(err)
				}
			}
			var run *db.Run
			deadline = time.Now().Add(15 * time.Second)
			for {
				run, err = database.GetRun(result.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status.Terminal() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("run did not become terminal: %+v", run)
				}
				time.Sleep(10 * time.Millisecond)
			}
			workDir := p.WorktreeDir(repo.ID, result.RunID)
			if action == "new_push" {
				if run.Status != types.RunCancelled || run.Error == nil || *run.Error != types.RunCancelReasonSuperseded {
					t.Fatalf("unexpected supersession: %+v", run)
				}
				assertProtectedWorktreePreserved(t, workDir, head)
				cleanupOrphanWorktrees(database, p, nil)
				assertProtectedWorktreePreserved(t, workDir, head)
				return
			}
			deadline = time.Now().Add(15 * time.Second)
			for {
				if _, err := os.Stat(workDir); os.IsNotExist(err) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("explicit operator abort no longer cleans its worktree")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestProtectedPathRefusalSurvivesShutdownCleanup(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{protectedPathCommitStep{step: &steps.PushStep{}}}
	})
	repo, head := setupTestGitRepo(t, p, database, "protected-shutdown")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: head,
	}, &result); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := database.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.AwaitingAgentSince != nil {
			break
		}
		if run.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("refusal did not park: %+v", run)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil); err != nil {
		t.Fatal(err)
	}
	client.Close()
	deadline = time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(p.Socket()); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated daemon did not shut down")
		}
		time.Sleep(10 * time.Millisecond)
	}
	workDir := p.WorktreeDir(repo.ID, result.RunID)
	assertProtectedWorktreePreserved(t, workDir, head)
	cleanupOrphanWorktrees(database, p, nil)
	assertProtectedWorktreePreserved(t, workDir, head)
}

func TestProtectedPathRefusalSurvivesFailedTrustedRecovery(t *testing.T) {
	for _, step := range []types.StepName{types.StepPush, types.StepCI} {
		t.Run(string(step), func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo, head := setupTestGitRepo(t, p, database, "protected-crash")
			run, err := database.InsertRun(repo.ID, "main", head, head)
			if err != nil {
				t.Fatal(err)
			}
			workDir := p.WorktreeDir(repo.ID, run.ID)
			if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), workDir, head); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if step == types.StepCI {
				if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
					t.Fatal(err)
				}
			}
			sr, err := database.InsertStepResult(run.ID, step)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(sr.ID); err != nil {
				t.Fatal(err)
			}
			sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: workDir, Run: run, DB: database, Config: config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), Log: func(string) {}}
			_, refusal := (protectedPathCommitStep{step: &steps.PushStep{}}).Execute(sctx)
			outcome := pipeline.ProtectedPathOutcome(refusal)
			if outcome == nil {
				t.Fatalf("expected protected-path refusal: %v", refusal)
			}
			if _, err := database.InsertStepRound(sr.ID, 1, "initial", &outcome.Findings, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, &outcome.Findings); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, workDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "unavailable.git"))
			mgr := NewRunManager(database, p, func() []pipeline.Step {
				if step == types.StepCI {
					return []pipeline.Step{&steps.CIStep{}}
				}
				return []pipeline.Step{&steps.PushStep{}}
			})
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mgr.prepareRecoveredRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "disable_project_settings") {
				t.Fatalf("recovery must fail closed at trusted config: %v", err)
			}
			layout, err := validatedWorktreeLayout(database, p, config.DefaultGlobalConfig())
			if err != nil {
				t.Fatal(err)
			}
			recoverOnStartup(database, p, mgr, layout)
			assertProtectedWorktreePreserved(t, workDir, head)
			cleanupOrphanWorktrees(database, p, nil)
			assertProtectedWorktreePreserved(t, workDir, head)
			if len(mgr.executors) != 0 {
				t.Fatal("failed trusted config launched an executor")
			}
		})
	}
}

func assertProtectedWorktreePreserved(t *testing.T, workDir, head string) {
	t.Helper()
	if _, err := os.Stat(workDir); err != nil {
		t.Fatalf("cleanup removed the refused worktree: %v", err)
	}
	if got := gitOutput(t, workDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("refusal changed HEAD: %s", got)
	}
	if got := gitOutput(t, workDir, "show", ":test.txt"); got != "staged edit" {
		t.Fatalf("refusal changed index: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, "test.txt")); err != nil || string(got) != "unstaged edit\n" {
		t.Fatalf("refusal changed working file: %q, %v", got, err)
	} else {
		t.Logf("retained Git state: HEAD=%s index:test.txt=%q worktree:test.txt=%q", gitOutput(t, workDir, "rev-parse", "HEAD"), gitOutput(t, workDir, "show", ":test.txt"), string(got))
	}
}

func (s *protectedPathPushRetryStep) Name() types.StepName { return types.StepPush }
func (s *protectedPathPushRetryStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if len(sctx.Config.ProtectedPaths) != 1 || sctx.Config.ProtectedPaths[0] != "*.txt" {
		return nil, fmt.Errorf("trusted protected_paths lost to pushed config: %q", sctx.Config.ProtectedPaths)
	}
	if !s.edited {
		s.edited = true
		if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, sctx.Run.HeadSHA); err != nil {
			return nil, err
		}
		return (protectedPathCommitStep{step: &steps.PushStep{}}).Execute(sctx)
	}
	return (&steps.PushStep{}).Execute(sctx)
}

func TestProtectedPathPushApprovalCannotSkipPublicationOrDiscardEdits(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&protectedPathPushRetryStep{}}
	})
	repo, headSHA := setupTestGitRepo(t, p, database, "protected-publication")
	// Exercise the manager's real trusted-config fetch: the pushed branch tries
	// to remove protection even though the trusted branch opts into repo commands.
	configFile := filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")
	if err := os.WriteFile(configFile, []byte("protected_paths: ['*.txt']\nallow_repo_commands: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "protect text files on trusted main")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	if err := os.WriteFile(configFile, []byte("protected_paths: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "try to remove protection on feature")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature")
	headSHA = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	t.Logf("trusted main config:\n%s\npushed feature config:\n%s", gitOutput(t, p.RepoDir(repo.ID), "show", "refs/heads/main:.no-mistakes.yaml"), gitOutput(t, p.RepoDir(repo.ID), "show", "refs/heads/feature:.no-mistakes.yaml"))
	publicationDir := filepath.Join(t.TempDir(), "published.git")
	gitCmd(t, "", "init", "--bare", publicationDir)
	if _, err := database.UpdateRepoForkURL(repo.ID, publicationDir); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/feature",
		Old: strings.Repeat("0", 40), New: headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}
	workDir := p.WorktreeDir(repo.ID, result.RunID)
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := database.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.AwaitingAgentSince != nil {
			break
		}
		if run.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("refusal did not park: %+v", run)
		}
		time.Sleep(10 * time.Millisecond)
	}

	approvalErr := client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID: result.RunID, Step: types.StepPush, Action: types.ActionApprove,
	}, nil)
	if approvalErr == nil {
		run := waitForRunTerminalState(t, database, result.RunID)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(workDir); os.IsNotExist(err) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, publicationErr := git.Run(context.Background(), publicationDir, "show-ref", "--verify", "refs/heads/feature")
		_, worktreeErr := os.Stat(workDir)
		t.Fatalf("approval bypassed refused Push: run=%s published=%v worktree_exists=%v", run.Status, publicationErr == nil, worktreeErr == nil)
	}
	if !strings.Contains(approvalErr.Error(), "protected") || !strings.Contains(approvalErr.Error(), "fix") {
		t.Fatalf("approval refusal lacks retry guidance: %v", approvalErr)
	}
	t.Logf("respond approve: %v", approvalErr)
	if got := gitOutput(t, workDir, "show", ":test.txt"); got != "staged edit" {
		t.Fatalf("approval changed the index: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, "test.txt")); err != nil || string(got) != "unstaged edit\n" {
		t.Fatalf("approval changed working files: %q, %v", got, err)
	}
	if _, err := git.Run(context.Background(), publicationDir, "show-ref", "--verify", "refs/heads/feature"); err == nil {
		t.Fatal("unresolved protected edit was published")
	}
	assertProtectedWorktreePreserved(t, workDir, headSHA)

	// The operator resolves the protected edit, then explicitly retries Push.
	gitCmd(t, workDir, "restore", "--source=HEAD", "--staged", "--worktree", "--", "test.txt")
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID: result.RunID, Step: types.StepPush, Action: types.ActionFix,
	}, nil); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, database, result.RunID)
	if run.Status != types.RunCompleted || run.LastPushedSHA == nil || *run.LastPushedSHA != headSHA {
		t.Fatalf("retry did not complete publication: %+v", run)
	}
	if got := gitOutput(t, publicationDir, "rev-parse", "refs/heads/feature"); got != headSHA {
		t.Fatalf("published head = %s, want %s", got, headSHA)
	}
	t.Logf("after explicit resolution + respond fix: run=%s last_pushed_sha=%s bare-remote feature=%s", run.Status, *run.LastPushedSHA, gitOutput(t, publicationDir, "rev-parse", "refs/heads/feature"))
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(workDir); os.IsNotExist(err) {
			t.Logf("after successful publication: worktree stat=%v", err)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("clean worktree was not removed after successful publication")
}

type protectedPathCommitStep struct {
	step pipeline.Step
}

func (s protectedPathCommitStep) Name() types.StepName { return s.step.Name() }
func (s protectedPathCommitStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	sctx.Config.ProtectedPaths = []string{"*.txt"}
	file := filepath.Join(sctx.WorkDir, "test.txt")
	if err := os.WriteFile(file, []byte("staged edit\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "test.txt"); err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte("unstaged edit\n"), 0o644); err != nil {
		return nil, err
	}
	if s.Name() == types.StepTest {
		sctx.Fixing = true
		sctx.Agent = recoveredRunTestAgent{}
	}
	return s.step.Execute(sctx)
}

func TestProtectedPathRefusalParksBeforeManagerCleanup(t *testing.T) {
	for _, step := range []pipeline.Step{&steps.PushStep{}, &steps.TestStep{}} {
		t.Run(string(step.Name()), func(t *testing.T) {
			p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
				return []pipeline.Step{protectedPathCommitStep{step: step}}
			})
			_, headSHA := setupTestGitRepo(t, p, database, "protected-paths")
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir("protected-paths"), Ref: "refs/heads/main",
				Old: strings.Repeat("0", 40), New: headSHA,
			}, &result); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				run, err := database.GetRun(result.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status != types.RunRunning && run.Status != types.RunPending {
					t.Fatalf("refusal reached terminal cleanup: status=%s error=%v", run.Status, run.Error)
				}
				if run.AwaitingAgentSince != nil {
					workDir := p.WorktreeDir(run.RepoID, run.ID)
					if got := gitOutput(t, workDir, "rev-parse", "HEAD"); got != headSHA {
						t.Errorf("refusal committed: HEAD=%s want %s", got, headSHA)
					}
					if got := gitOutput(t, workDir, "show", ":test.txt"); got != "staged edit" {
						t.Errorf("refusal changed index content: %q", got)
					}
					if got, err := os.ReadFile(filepath.Join(workDir, "test.txt")); err != nil || string(got) != "unstaged edit\n" {
						t.Errorf("refusal changed worktree content: %q err=%v", got, err)
					}
					results, err := database.GetStepsByRun(run.ID)
					if err != nil || len(results) != 1 || results[0].FindingsJSON == nil {
						t.Fatalf("missing persisted gate: %v %v", results, err)
					}
					findings, err := types.ParseFindingsJSON(*results[0].FindingsJSON)
					if err != nil || len(findings.Items) != 1 || findings.Items[0].File != "test.txt" || findings.Items[0].Action != types.ActionAskUser || !strings.Contains(findings.Items[0].Description, `rule "*.txt"`) {
						t.Fatalf("gate lost path, rule, or operator decision: %+v err=%v", findings, err)
					}
					if err := pipeline.ValidateRecoveredRun(database, run, []pipeline.Step{step}); err != nil {
						t.Fatalf("refusal gate cannot recover: %v", err)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("refusal never parked: %+v", run)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

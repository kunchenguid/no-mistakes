package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRecoverOnStartup_ResumesCompletedStepPrefixAtEveryCrashBoundary(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	stepNames := []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepVerify,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}

	for completedPrefix := 1; completedPrefix <= len(stepNames); completedPrefix++ {
		completedPrefix := completedPrefix
		t.Run(string(stepNames[completedPrefix-1]), func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			repo, headSHA := setupTestGitRepo(t, p, database, "crash-boundary-repo")
			run, err := database.InsertRun(repo.ID, "main", headSHA, headSHA)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			worktree := p.WorktreeDir(repo.ID, run.ID)
			if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
				t.Fatal(err)
			}

			steps := make([]pipeline.Step, 0, len(stepNames))
			probes := make([]*mockPassStep, 0, len(stepNames))
			for index, name := range stepNames {
				probe := &mockPassStep{name: name}
				steps = append(steps, probe)
				probes = append(probes, probe)
				result, err := database.InsertStepResult(run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				if index < completedPrefix {
					status := types.StepStatusCompleted
					if name == types.StepDocument {
						status = types.StepStatusSkipped
					}
					if status == types.StepStatusCompleted {
						if err := database.StartStep(result.ID); err != nil {
							t.Fatal(err)
						}
					}
					if err := database.CompleteStepWithStatus(result.ID, status, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			if completedPrefix > 5 {
				if _, err := database.CreateSeal(run.ID, headSHA, "lint"); err != nil {
					t.Fatal(err)
				}
			}

			manager := NewRunManager(database, p, func() []pipeline.Step { return steps })
			defer manager.Shutdown()
			recoverOnStartup(database, p, manager)

			recovered := waitForRunTerminalState(t, database, run.ID)
			if recovered.Status != types.RunCompleted {
				t.Fatalf("run recovered after %s = %s (error %v), want completed", stepNames[completedPrefix-1], recovered.Status, recovered.Error)
			}
			for index, probe := range probes {
				want := int32(1)
				if index < completedPrefix {
					want = 0
				}
				if got := probe.execCnt.Load(); got != want {
					t.Errorf("step %s executions = %d, want %d", probe.name, got, want)
				}
			}
		})
	}
}

func TestRecoverOnStartup_FinalizesAllTerminalRunWithoutRunnableAgent(t *testing.T) {
	t.Setenv("NM_DEMO", "0")
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	writeTestRoutingConfig(t, p, filepath.Join(root, "missing-agent"))
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo, headSHA := setupTestGitRepo(t, p, database, "all-terminal-recovery-repo")
	run, err := database.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), p.WorktreeDir(repo.ID, run.ID), headSHA); err != nil {
		t.Fatal(err)
	}
	result, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(result.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(result.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	ci := &mockPassStep{name: types.StepCI}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{ci} })
	defer manager.Shutdown()

	recoverOnStartup(database, p, manager)

	recovered := waitForRunTerminalState(t, database, run.ID)
	if recovered.Status != types.RunCompleted {
		t.Fatalf("all-terminal recovery = %s (error %v), want completed", recovered.Status, recovered.Error)
	}
	if got := ci.execCnt.Load(); got != 0 {
		t.Fatalf("all-terminal recovery replayed CI %d times", got)
	}
}

func TestRecoverOnStartup_PostPushContinuesWithoutRepublishing(t *testing.T) {
	t.Setenv("NM_DEMO", "0")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeTestRoutingConfig(t, p, executable)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo, headSHA := setupTestGitRepo(t, p, database, "post-push-recovery-repo")
	run, err := database.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateSeal(run.ID, headSHA, "lint"); err != nil {
		t.Fatal(err)
	}

	push := &mockPassStep{name: types.StepPush}
	pr := &mockPassStep{name: types.StepPR}
	ci := &mockPassStep{name: types.StepCI}
	steps := []pipeline.Step{push, pr, ci}
	for index, step := range steps {
		result, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if err := database.StartStep(result.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteStepWithStatus(result.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		}
	}

	manager := NewRunManager(database, p, func() []pipeline.Step { return steps })
	defer manager.Shutdown()
	recoverOnStartup(database, p, manager)

	recovered := waitForRunTerminalState(t, database, run.ID)
	if recovered.Status != types.RunCompleted {
		t.Fatalf("post-Push recovery = %s (error %v), want completed", recovered.Status, recovered.Error)
	}
	if got := push.execCnt.Load(); got != 0 {
		t.Fatalf("Push executions after durable completion = %d, want 0", got)
	}
	if got := pr.execCnt.Load(); got != 1 {
		t.Errorf("PR executions = %d, want 1", got)
	}
	if got := ci.execCnt.Load(); got != 1 {
		t.Errorf("CI executions = %d, want 1", got)
	}
	seal, err := database.LatestSeal(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seal == nil || seal.SHA != headSHA {
		t.Fatalf("publication seal = %+v, want SHA %s", seal, headSHA)
	}
}

func TestRecoverOnStartup_FailsClosedForUnsealedPublishedPrefix(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo, headSHA := setupTestGitRepo(t, p, database, "unsealed-published-prefix-repo")
	run, err := database.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), p.WorktreeDir(repo.ID, run.ID), headSHA); err != nil {
		t.Fatal(err)
	}
	pushResult, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(pushResult.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(pushResult.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepPR); err != nil {
		t.Fatal(err)
	}
	push := &mockPassStep{name: types.StepPush}
	pr := &mockPassStep{name: types.StepPR}
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{push, pr}
	})
	defer manager.Shutdown()

	recoverOnStartup(database, p, manager)

	recovered := waitForRunTerminalState(t, database, run.ID)
	if recovered.Status != types.RunFailed {
		t.Fatalf("unsealed published prefix status = %s, want failed", recovered.Status)
	}
	if push.execCnt.Load() != 0 || pr.execCnt.Load() != 0 {
		t.Fatalf("unsealed published prefix replayed steps: push=%d pr=%d", push.execCnt.Load(), pr.execCnt.Load())
	}
}

func TestRecoverOnStartup_FailsClosedForIncompleteTerminalPrefix(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo, headSHA := setupTestGitRepo(t, p, database, "invalid-prefix-repo")
	run, err := database.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), p.WorktreeDir(repo.ID, run.ID), headSHA); err != nil {
		t.Fatal(err)
	}
	completed, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(completed.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepTest); err != nil {
		t.Fatal(err)
	}
	review := &mockPassStep{name: types.StepReview}
	testStep := &mockPassStep{name: types.StepTest}
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{review, testStep}
	})
	defer manager.Shutdown()

	recoverOnStartup(database, p, manager)

	recovered := waitForRunTerminalState(t, database, run.ID)
	if recovered.Status != types.RunFailed {
		t.Fatalf("invalid recovered prefix status = %s, want failed", recovered.Status)
	}
	if recovered.Error == nil || *recovered.Error != "daemon crashed during execution" {
		t.Fatalf("invalid recovered prefix error = %v, want crash recovery failure", recovered.Error)
	}
	if review.execCnt.Load() != 0 || testStep.execCnt.Load() != 0 {
		t.Fatalf("invalid recovered prefix replayed steps: review=%d test=%d", review.execCnt.Load(), testStep.execCnt.Load())
	}
}

func TestRunWithOptions_RecoveryFailurePreventsServing(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	preparedRepo, headSHA := setupTestGitRepo(t, p, database, "prepared-recovery-repo")
	preparedRun, err := database.InsertRun(preparedRepo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(preparedRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(preparedRepo.ID), p.WorktreeDir(preparedRepo.ID, preparedRun.ID), headSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(preparedRun.ID, types.StepReview); err != nil {
		t.Fatal(err)
	}
	staleRepo, err := database.InsertRepoWithID("stale-recovery-repo", "/missing", "https://example.com/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	staleRun, err := database.InsertRun(staleRepo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(staleRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	faultDB, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_stale_run_recovery
		BEFORE UPDATE OF status ON runs
		WHEN NEW.status = 'failed'
		BEGIN
			SELECT RAISE(FAIL, 'injected stale-run recovery failure');
		END;
	`); err != nil {
		_ = faultDB.Close()
		t.Fatal(err)
	}
	if err := faultDB.Close(); err != nil {
		t.Fatal(err)
	}

	review := &mockPassStep{name: types.StepReview}
	result := make(chan error, 1)
	go func() {
		result <- RunWithOptions(p, database, func() []pipeline.Step { return []pipeline.Step{review} })
	}()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case runErr := <-result:
			if runErr == nil {
				t.Fatal("daemon startup succeeded after recovery failed")
			}
			if !strings.Contains(runErr.Error(), "recover stale runs") {
				t.Fatalf("startup error = %v, want stale-run recovery failure", runErr)
			}
			if _, statErr := os.Stat(p.Socket()); !os.IsNotExist(statErr) {
				t.Fatalf("daemon served after recovery failure: socket stat error %v", statErr)
			}
			prepared, getErr := database.GetRun(preparedRun.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			stale, getErr := database.GetRun(staleRun.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if prepared.Status != types.RunRunning || stale.Status != types.RunRunning {
				t.Fatalf("failed recovery transaction statuses = prepared %s, stale %s; want rollback to running", prepared.Status, stale.Status)
			}
			if got := review.execCnt.Load(); got != 0 {
				t.Fatalf("prepared run resumed %d steps before recovery transaction committed", got)
			}
			return
		case <-poll.C:
			if _, statErr := os.Stat(p.Socket()); statErr == nil {
				client, dialErr := ipc.Dial(p.Socket())
				if dialErr == nil {
					_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
					_ = client.Close()
				}
				<-result
				t.Fatal("daemon bound its IPC socket after recovery failed")
			}
		case <-deadline.C:
			t.Fatal("daemon neither failed startup nor served within deadline")
		}
	}
}

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	headAdvanceCrashRootEnv  = "NM_TEST_HEAD_ADVANCE_CRASH_ROOT"
	headAdvanceCrashPhaseEnv = "NM_TEST_HEAD_ADVANCE_CRASH_PHASE"
)

type headAdvanceCrashFixture struct {
	RunID     string `json:"run_id"`
	RepoID    string `json:"repo_id"`
	Branch    string `json:"branch"`
	OldHead   string `json:"old_head"`
	Candidate string `json:"candidate"`
	AnchorRef string `json:"anchor_ref"`
}

// TestPreparedHeadAdvanceSurvivesProcessRestart exercises the three durable
// crash boundaries with two separate OS processes. The child prepares the
// exact Git/DB state and exits without cleanup; the parent starts a fresh
// daemon process lifetime against the same NM_HOME and observes startup
// reconciliation before stale-run failure and worktree cleanup.
func TestPreparedHeadAdvanceSurvivesProcessRestart(t *testing.T) {
	if root := os.Getenv(headAdvanceCrashRootEnv); root != "" {
		if err := stageHeadAdvanceCrash(root, os.Getenv(headAdvanceCrashPhaseEnv)); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "stage head-advance crash: %v\n", err)
			os.Exit(2)
		}
		os.Exit(0)
	}

	for _, phase := range []string{"after_anchor", "after_gate", "after_db"} {
		t.Run(phase, func(t *testing.T) {
			root, err := os.MkdirTemp("", "nm-head-restart-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })

			cmd := exec.Command(os.Args[0], "-test.run=^TestPreparedHeadAdvanceSurvivesProcessRestart$")
			cmd.Env = append(os.Environ(), headAdvanceCrashRootEnv+"="+root, headAdvanceCrashPhaseEnv+"="+phase)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("crash-staging child failed: %v\n%s", err, output)
			}

			fixtureData, err := os.ReadFile(filepath.Join(root, "head-advance-crash.json"))
			if err != nil {
				t.Fatal(err)
			}
			var fixture headAdvanceCrashFixture
			if err := json.Unmarshal(fixtureData, &fixture); err != nil {
				t.Fatal(err)
			}

			p := paths.WithRoot(root)
			var daemonOutput bytes.Buffer
			daemonCmd := exec.Command(os.Args[0])
			daemonCmd.Env = append(os.Environ(), "NM_DAEMON_HELPER_PROCESS=daemon", "NM_HOME="+root, "NO_MISTAKES_TELEMETRY=0", "NO_MISTAKES_NO_UPDATE_CHECK=1")
			daemonCmd.Stdout = &daemonOutput
			daemonCmd.Stderr = &daemonOutput
			if err := daemonCmd.Start(); err != nil {
				t.Fatal(err)
			}
			errCh := make(chan error, 1)
			go func() { errCh <- daemonCmd.Wait() }()
			waitForHeadAdvanceTestDaemon(t, p.Socket(), errCh, &daemonOutput)
			defer stopHeadAdvanceTestDaemon(t, p.Socket(), errCh)

			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			run, err := database.GetRun(fixture.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run == nil || run.Status != types.RunFailed {
				t.Fatalf("stale run status = %#v, want failed", run)
			}

			wantDurableHead := fixture.OldHead
			if phase != "after_anchor" {
				wantDurableHead = fixture.Candidate
			}
			if run.HeadSHA != wantDurableHead {
				t.Fatalf("durable run head = %s, want %s", run.HeadSHA, wantDurableHead)
			}
			gateDir := p.RepoDir(fixture.RepoID)
			gateHead, err := gitpkg.Run(context.Background(), gateDir, "rev-parse", "--verify", "refs/heads/"+fixture.Branch+"^{commit}")
			if err != nil {
				t.Fatal(err)
			}
			if gateHead != wantDurableHead {
				t.Fatalf("gate head = %s, want %s", gateHead, wantDurableHead)
			}
			anchor, err := gitpkg.Run(context.Background(), gateDir, "rev-parse", "--verify", fixture.AnchorRef+"^{commit}")
			if err != nil || anchor != fixture.Candidate {
				t.Fatalf("candidate anchor = %q, %v; want %s", anchor, err, fixture.Candidate)
			}
			journal, err := database.GetActiveRunHeadAdvance(fixture.RunID, fixture.Candidate)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "after_anchor" && journal != nil {
				t.Fatalf("anchor-only crash unexpectedly persisted journal: %#v", journal)
			}
			if phase != "after_anchor" && journal == nil {
				t.Fatal("gate/DB crash lost exact prepared journal")
			}
			if _, err := os.Stat(p.WorktreeDir(fixture.RepoID, fixture.RunID)); !os.IsNotExist(err) {
				t.Fatalf("terminal stale-run worktree was not cleaned up: %v", err)
			}
		})
	}
}

func stageHeadAdvanceCrash(root, phase string) error {
	switch phase {
	case "after_anchor", "after_gate", "after_db":
	default:
		return fmt.Errorf("unsupported crash phase %q", phase)
	}
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		return err
	}

	workDir, err := os.MkdirTemp(root, "source-")
	if err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(workDir, "init"); err != nil {
		return err
	}
	for _, args := range [][]string{{"config", "user.email", "test@test.com"}, {"config", "user.name", "Test"}} {
		if _, err := runHeadAdvanceTestGit(workDir, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "source.txt"), []byte("old\n"), 0o644); err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(workDir, "add", "source.txt"); err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(workDir, "commit", "-m", "old head"); err != nil {
		return err
	}
	oldHead, err := runHeadAdvanceTestGit(workDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	const repoID = "restart-head-custody"
	const branch = "feature"
	gateDir := p.RepoDir(repoID)
	if _, err := runHeadAdvanceTestGit("", "init", "--bare", gateDir); err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(workDir, "remote", "add", "gate", gateDir); err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(workDir, "push", "gate", "HEAD:refs/heads/main", "HEAD:refs/heads/"+branch); err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(gateDir, "remote", "add", "origin", gateDir); err != nil {
		return err
	}

	database, err := db.Open(p.DB())
	if err != nil {
		return err
	}
	repo, err := database.InsertRepoWithID(repoID, workDir, "https://github.com/test/restart-head-custody", "main")
	if err != nil {
		return err
	}
	run, err := database.InsertRun(repo.ID, branch, oldHead, oldHead)
	if err != nil {
		return err
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		return err
	}
	step, err := database.InsertStepResult(run.ID, types.StepLint)
	if err != nil {
		return err
	}
	if err := database.StartStep(step.ID); err != nil {
		return err
	}

	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), gateDir, worktree, oldHead); err != nil {
		return err
	}
	for _, args := range [][]string{{"config", "user.email", "test@test.com"}, {"config", "user.name", "Test"}} {
		if _, err := runHeadAdvanceTestGit(worktree, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(worktree, "add", "candidate.txt"); err != nil {
		return err
	}
	if _, err := runHeadAdvanceTestGit(worktree, "commit", "-m", "candidate head"); err != nil {
		return err
	}
	candidate, err := runHeadAdvanceTestGit(worktree, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	anchorRef := "refs/no-mistakes/run-head-candidates/" + run.ID + "/" + candidate
	if _, err := gitpkg.Run(context.Background(), worktree, "update-ref", anchorRef, candidate, strings.Repeat("0", 40)); err != nil {
		return err
	}

	advance := db.ActiveRunHeadAdvance{
		RunID:        run.ID,
		RepoID:       repo.ID,
		Branch:       branch,
		StepName:     string(types.StepLint),
		ExpectedHead: oldHead,
		Candidate:    candidate,
		AnchorRef:    anchorRef,
	}
	if phase != "after_anchor" {
		if err := database.PrepareActiveRunHeadAdvanceCAS(advance); err != nil {
			return err
		}
		if _, err := gitpkg.Run(context.Background(), gateDir, "update-ref", "refs/heads/"+branch, candidate, oldHead); err != nil {
			return err
		}
	}
	if phase == "after_db" {
		if err := database.AdvanceActiveRunHeadCAS(advance); err != nil {
			return err
		}
	}

	fixtureData, err := json.Marshal(headAdvanceCrashFixture{
		RunID: run.ID, RepoID: repo.ID, Branch: branch, OldHead: oldHead,
		Candidate: candidate, AnchorRef: anchorRef,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "head-advance-crash.json"), fixtureData, 0o644)
}

func runHeadAdvanceTestGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func waitForHeadAdvanceTestDaemon(t *testing.T, socket string, errCh <-chan error, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("restarted daemon exited before readiness: %v\n%s", err, output.String())
		default:
		}
		client, err := ipc.Dial(socket)
		if err == nil {
			var health ipc.HealthResult
			err = client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second)
			_ = client.Close()
			if err == nil && health.Status == "ok" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("restarted daemon did not become ready")
}

func stopHeadAdvanceTestDaemon(t *testing.T, socket string, errCh <-chan error) {
	t.Helper()
	client, err := ipc.Dial(socket)
	if err == nil {
		_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &ipc.ShutdownResult{})
		_ = client.Close()
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("restarted daemon shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("restarted daemon did not stop")
	}
}

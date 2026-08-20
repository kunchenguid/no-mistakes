//go:build e2e

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	pipelinesteps "github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type failAfterProductionStep struct {
	step pipeline.Step
	live *atomic.Bool
}

type cancelOnceProductionStep struct {
	step pipeline.Step
	live *atomic.Bool
}

type failThenBlockProductionStep struct {
	step    pipeline.Step
	calls   atomic.Int32
	started chan<- struct{}
}

type deliveryResumeDaemonProcess struct {
	cmd  *exec.Cmd
	done <-chan struct{}
}

func (s *failThenBlockProductionStep) Name() types.StepName { return s.step.Name() }

func (s *failThenBlockProductionStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	outcome, err := s.step.Execute(ctx)
	if err != nil {
		return nil, err
	}
	switch s.calls.Add(1) {
	case 1:
		return nil, errors.New("injected recoverable post-delivery failure")
	case 2:
		select {
		case s.started <- struct{}{}:
		default:
		}
		<-ctx.Ctx.Done()
		return nil, ctx.Ctx.Err()
	default:
		return outcome, nil
	}
}

func (s *cancelOnceProductionStep) Name() types.StepName { return s.step.Name() }

func (s *cancelOnceProductionStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if !s.live.CompareAndSwap(true, false) {
		return s.step.Execute(ctx)
	}
	cancelled := *ctx
	cancelledCtx, cancelledCancel := context.WithCancel(ctx.Ctx)
	cancelled.Ctx = cancelledCtx
	cancelledCancel()
	return s.step.Execute(&cancelled)
}

func (s *failAfterProductionStep) Name() types.StepName { return s.step.Name() }

func (s *failAfterProductionStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	outcome, err := s.step.Execute(ctx)
	if err != nil {
		return nil, err
	}
	if s.live.CompareAndSwap(true, false) {
		return nil, errors.New("injected recoverable post-delivery failure")
	}
	return outcome, nil
}

func TestDeliveryResumeUsesProductionDeliverySteps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider script is POSIX-only")
	}
	for _, failAt := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		t.Run(string(failAt), func(t *testing.T) {
			providerDir := t.TempDir()
			installDeliveryResumeGH(t, providerDir)
			t.Setenv("PATH", providerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			probe := &deliveryResumeProbe{}
			var failureLive atomic.Bool
			failureLive.Store(true)
			p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
				production := map[types.StepName]pipeline.Step{
					types.StepPush: &pipelinesteps.PushStep{},
					types.StepPR:   &pipelinesteps.PRStep{},
					types.StepCI:   &pipelinesteps.CIStep{},
				}
				result := make([]pipeline.Step, 0, len(types.AllSteps()))
				for _, name := range types.AllSteps() {
					if step := production[name]; step != nil {
						if name == failAt {
							if name == types.StepCI {
								step = &cancelOnceProductionStep{step: step, live: &failureLive}
							} else {
								step = &failAfterProductionStep{step: step, live: &failureLive}
							}
						}
						result = append(result, step)
						continue
					}
					result = append(result, &deliveryResumeProbeStep{name: name, probe: probe})
				}
				return result
			})

			repoID := "production-delivery-resume-" + string(failAt)
			_, headSHA := setupTestGitRepo(t, p, database, repoID)
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			var first ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir(repoID), Ref: "refs/heads/feature", Old: strings.Repeat("0", 40), New: headSHA,
			}, &first); err != nil {
				t.Fatal(err)
			}
			if run := waitForRunTerminalState(t, database, first.RunID); run.Status != types.RunFailed {
				t.Fatalf("source status = %s, want failed", run.Status)
			}
			var retry ipc.RerunResult
			if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repoID, Branch: "feature"}, &retry); err != nil {
				t.Fatal(err)
			}
			if run := waitForRunTerminalState(t, database, retry.RunID); run.Status != types.RunCompleted {
				t.Fatalf("retry status = %s, want completed: %v", run.Status, run.Error)
			}
			for _, name := range validationStepNamesForTest() {
				if got := probe.calls[name.Order()-1].Load(); got != 1 {
					t.Errorf("validation step %s executed %d times, want 1", name, got)
				}
			}
		})
	}
}

func TestDeliveryResumeSurvivesDaemonRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider script is POSIX-only")
	}
	root, err := os.MkdirTemp("/tmp", "nm-dr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	providerDir := t.TempDir()
	installDeliveryResumeGH(t, providerDir)
	blockPath := filepath.Join(providerDir, "block-pr")
	if err := os.WriteFile(blockPath, []byte("block\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	const repoID = "restart-delivery-resume"
	repo, _ := setupTestGitRepo(t, p, database, repoID)
	repoConfig := "allow_repo_commands: true\ncommands:\n  format: 'while [ -f " + blockPath + " ]; do sleep 0.05; done'\n"
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(repoConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "configure blocking delivery fixture")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	baseSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "feature.txt"), []byte("delivery recovery\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "feature.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "add delivery fixture")
	headSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/nm-test/delivery-head")

	start := func() *deliveryResumeDaemonProcess {
		t.Helper()
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^$")
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.Env = append(os.Environ(),
			"NM_DAEMON_HELPER_PROCESS=delivery-resume-daemon",
			"NM_HOME="+root,
			"DELIVERY_RESUME_GH_BLOCK="+blockPath,
			"PATH="+providerDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		daemon := &deliveryResumeDaemonProcess{cmd: cmd, done: done}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-done:
				t.Fatalf("delivery resume daemon exited before readiness: %v", cmd.ProcessState)
			default:
			}
			client, err := ipc.Dial(p.Socket())
			if err == nil {
				var health ipc.HealthResult
				err = client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &health)
				_ = client.Close()
				if err == nil && health.Status == "ok" {
					return daemon
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		killDeliveryResumeDaemon(daemon)
		t.Fatal("delivery resume daemon did not become ready")
		return nil
	}
	firstDaemon := start()
	t.Cleanup(func() { killDeliveryResumeDaemon(firstDaemon) })
	if err := os.WriteFile(filepath.Join(p.RepoDir(repoID), "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var pushed ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repoID), Ref: "refs/heads/feature", Old: baseSHA, New: headSHA,
	}, &pushed); err != nil {
		t.Fatal(err)
	}
	client.Close()
	waitForCheckpointAndStep(t, database, pushed.RunID, types.StepPush)
	beforeRestart, err := database.GetStepsByRun(pushed.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reviewCompletedAt := beforeRestart[types.StepReview.Order()-1].CompletedAt
	killDeliveryResumeDaemon(firstDaemon)
	if err := os.Remove(blockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.RepoDir(repoID), "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature")
	secondDaemon := start()
	defer stopDeliveryResumeDaemon(t, p, secondDaemon)
	if run := waitForRunTerminalState(t, database, pushed.RunID); run.Status != types.RunCompleted {
		errorText := ""
		if run.Error != nil {
			errorText = *run.Error
		}
		t.Fatalf("recovered status = %s, want completed: %s", run.Status, errorText)
	}
	steps, err := database.GetStepsByRun(pushed.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if reviewCompletedAt == nil || steps[types.StepReview.Order()-1].CompletedAt == nil ||
		*steps[types.StepReview.Order()-1].CompletedAt != *reviewCompletedAt {
		t.Fatal("review completion evidence changed across delivery recovery")
	}
}

func TestConcurrentRerunsSupersedeDeliveryReuseSafely(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider script is POSIX-only")
	}
	providerDir := t.TempDir()
	installDeliveryResumeGH(t, providerDir)
	t.Setenv("PATH", providerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	probe := &deliveryResumeProbe{}
	blocked := make(chan struct{}, 1)
	push := &failThenBlockProductionStep{step: &pipelinesteps.PushStep{}, started: blocked}
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		result := make([]pipeline.Step, 0, len(types.AllSteps()))
		for _, name := range types.AllSteps() {
			switch name {
			case types.StepPush:
				result = append(result, push)
			case types.StepPR:
				result = append(result, &pipelinesteps.PRStep{})
			case types.StepCI:
				result = append(result, &pipelinesteps.CIStep{})
			default:
				result = append(result, &deliveryResumeProbeStep{name: name, probe: probe})
			}
		}
		return result
	})
	const repoID = "concurrent-delivery-resume"
	_, headSHA := setupTestGitRepo(t, p, database, repoID)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var source ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repoID), Ref: "refs/heads/feature", Old: strings.Repeat("0", 40), New: headSHA,
	}, &source); err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, database, source.RunID); run.Status != types.RunFailed {
		t.Fatalf("source status = %s, want failed", run.Status)
	}
	var first ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repoID, Branch: "feature"}, &first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("first recovery did not reach delivery")
	}
	var second ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repoID, Branch: "feature"}, &second); err != nil {
		t.Fatal(err)
	}
	if run := waitForInactiveRun(t, database, first.RunID); run.Status != types.RunFailed && run.Status != types.RunCancelled {
		t.Fatalf("superseded status = %s, want failed or cancelled", run.Status)
	}
	if run := waitForRunTerminalState(t, database, second.RunID); run.Status != types.RunCompleted {
		t.Fatalf("replacement status = %s, want completed: %v", run.Status, run.Error)
	}
	if got := probe.calls[types.StepReview.Order()-1].Load(); got != 2 {
		t.Fatalf("review executions = %d, want source plus fail-closed replacement", got)
	}
}

func waitForInactiveRun(t *testing.T, database *db.DB, runID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(runID)
		if err == nil && run != nil && run.Status != types.RunPending && run.Status != types.RunRunning {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not become inactive", runID)
	return nil
}

func waitForCheckpointAndStep(t *testing.T, database *db.DB, runID string, stepName types.StepName) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		checkpoint, _ := database.GetValidationCheckpoint(runID)
		steps, _ := database.GetStepsByRun(runID)
		if checkpoint != nil && len(steps) >= stepName.Order() && steps[stepName.Order()-1].Status == types.StepStatusRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	run, _ := database.GetRun(runID)
	steps, _ := database.GetStepsByRun(runID)
	checkpoint, _ := database.GetValidationCheckpoint(runID)
	t.Fatalf("run %s did not persist checkpoint and start %s: run=%#v checkpoint=%v steps=%#v", runID, stepName, run, checkpoint != nil, steps)
}

func stopDeliveryResumeDaemon(t *testing.T, p *paths.Paths, daemon *deliveryResumeDaemonProcess) {
	t.Helper()
	client, err := ipc.Dial(p.Socket())
	if err == nil {
		_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
		_ = client.Close()
	}
	select {
	case <-daemon.done:
	case <-time.After(5 * time.Second):
		_ = daemon.cmd.Process.Kill()
		t.Fatal("replacement daemon did not stop")
	}
}

func killDeliveryResumeDaemon(daemon *deliveryResumeDaemonProcess) {
	if daemon == nil {
		return
	}
	select {
	case <-daemon.done:
		return
	default:
	}
	_ = daemon.cmd.Process.Kill()
	<-daemon.done
}

func installDeliveryResumeGH(t *testing.T, dir string) {
	t.Helper()
	state := filepath.Join(dir, "pr-created")
	ciState := filepath.Join(dir, "ci-state")
	if err := os.WriteFile(ciState, []byte("MERGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
  "auth status"*) exit 0 ;;
  "pr list"*)
	while [ -n "$DELIVERY_RESUME_GH_BLOCK" ] && [ -f "$DELIVERY_RESUME_GH_BLOCK" ]; do sleep 0.05; done
    if [ -f ` + state + ` ]; then printf '%s\n' '[{"number":42,"url":"https://github.com/test/repo/pull/42","headRefName":"feature","headRepositoryOwner":{"login":"test"}}]'; else printf '%s\n' '[]'; fi
    exit 0 ;;
  "pr create"*) touch ` + state + `; printf '%s\n' 'https://github.com/test/repo/pull/42'; exit 0 ;;
  "pr edit"*) exit 0 ;;
  "pr view"*"--json state"*)
    state=$(cat ` + ciState + `)
    if [ "$state" = ERROR ]; then printf '%s\n' 'provider unavailable' >&2; exit 1; fi
    printf '%s\n' "$state"; exit 0 ;;
  "pr view"*"--json mergeable"*) printf '%s\n' 'MERGEABLE'; exit 0 ;;
  "pr checks"*) printf '%s\n' '[{"name":"build","state":"SUCCESS","bucket":"pass"}]'; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

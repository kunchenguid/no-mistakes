package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the wire field through admission and real Git. On older daemons an
// unknown base_remote is silently dropped, so the release branch is looked up
// on origin and this request fails before the observer can run.
func TestIntegrationRemoteFreshRunUsesSelectedRelease(t *testing.T) {
	seen := make(chan *db.Repo, 1)
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&steps.RebaseStep{}, &captureRefreshRepoStep{seen: seen}}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-remote")
	gateDir := p.RepoDir(repo.ID)
	custom := filepath.Join(t.TempDir(), "customer.git")
	gitCmd(t, "", "clone", "--bare", gateDir, custom)
	gitCmd(t, custom, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", gateDir)
	const customerURL = "https://github.com/customer/project.git"
	gitCmd(t, repo.WorkingPath, "remote", "add", "custom", customerURL)
	gitCmd(t, repo.WorkingPath, "config", "url."+custom+".insteadOf", customerURL)
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "customer sentinel")
	sentinel := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "custom", "HEAD:main", "HEAD:release/customer")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "topic", head)
	gitCmd(t, repo.WorkingPath, "push", "gate", "topic")
	gitCmd(t, gateDir, "update-ref", "refs/remotes/origin/main", head)
	originConfig := gitOutput(t, gateDir, "config", "--get-regexp", "remote.origin.")
	originRefs := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.StartFreshRunResult
	err = client.Call(ipc.MethodStartFreshRun, map[string]any{
		"repo_id": repo.ID, "branch": "topic", "head_sha": head,
		"intent": "integrate the customer release", "launch_nonce": "custom-release",
		"validation_generation": "one", "pr_base_branch": "release/customer", "base_remote": "custom",
	}, &result)
	if err != nil {
		t.Fatalf("selected custom remote must admit its release branch: %v", err)
	}
	run := waitForRunTerminalState(t, database, result.Receipt.RunID)
	if run.Status != types.RunCompleted || run.HeadSHA != sentinel {
		t.Fatalf("run = %+v, want completed at selected sentinel %s", run, sentinel)
	}
	got := <-seen
	if got.UpstreamURL != customerURL {
		t.Fatalf("integration destination = %q, want %q", got.UpstreamURL, customerURL)
	}
	if actual := gitOutput(t, gateDir, "config", "--get-regexp", "remote.origin."); actual != originConfig {
		t.Fatalf("origin configuration changed: %q -> %q", originConfig, actual)
	}
	if actual := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/"); actual != originRefs {
		t.Fatalf("origin refs changed: %q -> %q", originRefs, actual)
	}
	if _, err := database.GetRun(run.ID); err != nil {
		t.Fatal(err)
	}
}

func integrationRecoverySteps() []pipeline.Step {
	return []pipeline.Step{&steps.RebaseStep{}, &mockApprovalStep{name: types.StepReview}, &integrationRecoveryStep{}}
}

type integrationRecoveryStep struct{}

func (*integrationRecoveryStep) Name() types.StepName { return types.StepTest }
func (*integrationRecoveryStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	// Exercise an integration consumer after the restarted daemon releases
	// the parked review, then retain the destination the consumer actually saw.
	if _, err := (&steps.RebaseStep{}).Execute(sctx); err != nil {
		return nil, err
	}
	data, err := json.Marshal(sctx.Repo.UpstreamURL)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(os.Getenv("NM_INTEGRATION_RECOVERY_PROOF"), data, 0o600); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

func TestIntegrationRemoteSurvivesPrivateDaemonRestart(t *testing.T) {
	root, err := os.MkdirTemp("", "nm1237-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "integration-restart")
	gateDir := p.RepoDir(repo.ID)
	custom := filepath.Join(t.TempDir(), "customer.git")
	gitCmd(t, "", "clone", "--bare", gateDir, custom)
	gitCmd(t, custom, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", gateDir)
	gitCmd(t, repo.WorkingPath, "remote", "add", "custom", custom)
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "customer sentinel")
	sentinel := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "custom", "HEAD:main", "HEAD:release/customer")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "topic", head)
	gitCmd(t, repo.WorkingPath, "push", "gate", "topic")
	gitCmd(t, gateDir, "update-ref", "refs/remotes/origin/main", head)
	originRefs := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/")
	proof := filepath.Join(t.TempDir(), "after-restart.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var process *exec.Cmd
	start := func() {
		t.Helper()
		process = exec.Command(executable, "-test.run=^$")
		process.Env = upsertEnv(os.Environ(), "NM_DAEMON_HELPER_PROCESS", "integration-remote")
		process.Env = upsertEnv(process.Env, "NM_HOME", p.Root())
		process.Env = upsertEnv(process.Env, "NM_INTEGRATION_RECOVERY_PROOF", proof)
		process.Stdout, process.Stderr = os.Stdout, os.Stderr
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
		waitForDaemonReady(t, p)
	}
	defer func() {
		if process != nil && process.Process != nil {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	}()
	start()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var launched ipc.StartFreshRunResult
	err = client.Call(ipc.MethodStartFreshRun, map[string]any{
		"repo_id": repo.ID, "branch": "topic", "head_sha": head,
		"intent": "retain customer integration after a restart", "launch_nonce": "restart",
		"validation_generation": "one", "pr_base_branch": "release/customer", "base_remote": "custom",
	}, &launched)
	_ = client.Close()
	if err != nil {
		t.Fatal(err)
	}
	runID := launched.Receipt.RunID
	deadline := time.Now().Add(60 * time.Second)
	for {
		run, err := database.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.AwaitingAgentSince != nil {
			break
		}
		if run.Status == types.RunFailed || time.Now().After(deadline) {
			t.Fatalf("run did not park before restart: %+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = process.Wait()
	process = nil
	// Recovery must ignore a since-changed registration and reconstruct the
	// selected destination from this run's row.
	if _, err := database.ReplaceRepoURLs(repo.ID, "https://github.com/other/product.git", ""); err != nil {
		t.Fatal(err)
	}
	start()
	client, err = ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: runID, Step: types.StepReview, Action: types.ActionApprove}, &ipc.RespondResult{}); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, database, runID)
	if run.Status != types.RunCompleted || run.HeadSHA != sentinel || run.Integration == nil || run.Integration.URL != custom {
		t.Fatalf("restarted run lost its destination: %+v", run)
	}
	data, err := os.ReadFile(proof)
	if err != nil || string(data) != fmt.Sprintf("%q", custom) {
		t.Fatalf("post-restart destination = %s, %v", data, err)
	}
	if actual := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/"); actual != originRefs {
		t.Fatalf("restart rewrote origin refs: %s -> %s", originRefs, actual)
	}
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	process = nil
}

func TestIntegrationRemoteConcurrentRunsHaveSeparateDestinations(t *testing.T) {
	seen := make(chan *db.Repo, 2)
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&steps.RebaseStep{}, &captureRefreshRepoStep{seen: seen}}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-concurrent")
	gateDir := p.RepoDir(repo.ID)
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", gateDir)
	gitCmd(t, gateDir, "update-ref", "refs/remotes/origin/main", head)
	originRefs := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/")
	remotes := make([]string, 2)
	sentinels := make([]string, 2)
	for i := range 2 {
		name := fmt.Sprint("customer", i)
		remotes[i] = filepath.Join(t.TempDir(), name+".git")
		gitCmd(t, "", "clone", "--bare", gateDir, remotes[i])
		gitCmd(t, remotes[i], "symbolic-ref", "HEAD", "refs/heads/main")
		gitCmd(t, repo.WorkingPath, "remote", "add", name, remotes[i])
		gitCmd(t, repo.WorkingPath, "checkout", "--detach", head)
		gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", name+" sentinel")
		sentinels[i] = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
		gitCmd(t, repo.WorkingPath, "push", name, "HEAD:main", "HEAD:release/customer")
		gitCmd(t, repo.WorkingPath, "push", "gate", head+":refs/heads/topic-"+name)
	}
	type result struct {
		i     int
		runID string
		err   error
	}
	results := make(chan result, 2)
	for i := range 2 {
		go func() {
			client, err := ipc.Dial(p.Socket())
			var launched ipc.StartFreshRunResult
			if err == nil {
				defer client.Close()
				name := fmt.Sprint("customer", i)
				err = client.Call(ipc.MethodStartFreshRun, map[string]any{
					"repo_id": repo.ID, "branch": "topic-" + name, "head_sha": head,
					"intent": "keep concurrent integration targets separate", "launch_nonce": name,
					"validation_generation": "one", "pr_base_branch": "release/customer", "base_remote": name,
				}, &launched)
			}
			results <- result{i: i, runID: launched.Receipt.RunID, err: err}
		}()
	}
	keys := map[string]bool{}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		run := waitForRunTerminalState(t, database, result.runID)
		if run.Status != types.RunCompleted || run.HeadSHA != sentinels[result.i] || run.Integration == nil || run.Integration.URL != remotes[result.i] {
			t.Fatalf("run used another destination: %+v", run)
		}
		if keys[run.Integration.Key] {
			t.Fatal("concurrent runs shared an integration ref namespace")
		}
		keys[run.Integration.Key] = true
	}
	if actual := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/"); actual != originRefs {
		t.Fatalf("concurrent selections rewrote origin refs: %s -> %s", originRefs, actual)
	}
}

func TestIntegrationRemoteInvalidSelectionsRefuseBeforeRunCreation(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-invalid")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
	gitCmd(t, repo.WorkingPath, "remote", "add", "unreachable", filepath.Join(t.TempDir(), "absent.git"))
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i, remote := range []string{"missing", "unreachable", "--upload-pack=evil", "../origin", "custom\norigin"} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var result ipc.StartFreshRunResult
			err := client.Call(ipc.MethodStartFreshRun, map[string]any{
				"repo_id": repo.ID, "branch": "main", "head_sha": head,
				"intent": "refuse unresolved destinations", "launch_nonce": fmt.Sprint("invalid-", i),
				"validation_generation": "one", "pr_base_branch": "main", "base_remote": remote,
			}, &result)
			if err == nil {
				t.Fatalf("selector %q fell back to origin", remote)
			}
		})
	}
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("invalid selection created run records: %+v, %v", runs, err)
	}
}

func TestIntegrationRemoteUnsetStillUsesOrigin(t *testing.T) {
	seen := make(chan *db.Repo, 1)
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&captureRefreshRepoStep{seen: seen}}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-default")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
	gitCmd(t, repo.WorkingPath, "remote", "add", "custom", filepath.Join(t.TempDir(), "missing.git"))
	gitCmd(t, repo.WorkingPath, "config", "branch.main.remote", "custom")
	gitCmd(t, repo.WorkingPath, "config", "branch.main.merge", "refs/heads/main")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var launched ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{
		RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "retain the origin default",
		LaunchNonce: "default", ValidationGeneration: "one", PRBaseBranch: "main",
	}, &launched); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, database, launched.Receipt.RunID)
	if run.Status != types.RunCompleted || run.Integration != nil {
		t.Fatalf("unset selector inferred the tracking remote: %+v", run)
	}
	if got := <-seen; got.UpstreamURL != repo.UpstreamURL {
		t.Fatalf("unset selector changed destination: %+v", got)
	}
}

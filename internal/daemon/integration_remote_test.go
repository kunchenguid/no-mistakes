package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/integration"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the wire field through admission and real Git. On older daemons an
// unknown base_remote is silently dropped, so the release branch is looked up
// on origin and this request fails before the observer can run.
func TestIntegrationRemoteFreshRunUsesSelectedRelease(t *testing.T) {
	seen := make(chan *db.Repo, 1)
	forgeEnv, forgeLog := integrationFakeForge(t)
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{
			&steps.RebaseStep{}, &integrationReviewStep{seen: seen},
			&mockPassStep{name: types.StepTest}, &mockPassStep{name: types.StepDocument}, &mockPassStep{name: types.StepLint},
			&integrationEnvStep{step: &steps.PushStep{}, env: forgeEnv},
			&integrationEnvStep{step: &steps.PRStep{}, env: forgeEnv},
		}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-remote")
	gateDir := p.RepoDir(repo.ID)
	custom := filepath.Join(t.TempDir(), "customer.git")
	product := filepath.Join(t.TempDir(), "product.git")
	gitCmd(t, "", "clone", "--bare", gateDir, product)
	gitCmd(t, product, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, "", "clone", "--bare", gateDir, custom)
	gitCmd(t, custom, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", product)
	gitCmd(t, gateDir, "remote", "set-url", "origin", product)
	const customerURL = "https://github.com/customer/project.git"
	gitCmd(t, repo.WorkingPath, "remote", "add", "custom", customerURL)
	gitCmd(t, repo.WorkingPath, "config", "url."+custom+".insteadOf", customerURL)
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "customer sentinel")
	sentinel := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "custom", "HEAD:refs/heads/main", "HEAD:refs/heads/release/customer")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "topic", head)
	head = commitIntegrationTopic(t, repo.WorkingPath)
	gitCmd(t, repo.WorkingPath, "push", "gate", "topic")
	gitCmd(t, gateDir, "update-ref", "refs/remotes/origin/main", gitOutput(t, product, "rev-parse", "main"))
	originConfig := gitOutput(t, gateDir, "config", "--get-regexp", "remote.origin.")
	originRefs := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/")
	productRefs := gitOutput(t, product, "for-each-ref", "--format=%(refname) %(objectname)")

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
		"pr_title": "fix: customer release", "pr_body": "## What Changed\n\nFix the customer release.", "omit_intent": true,
	}, &result)
	if err != nil {
		t.Fatalf("selected custom remote must admit its release branch: %v", err)
	}
	run := waitForRunTerminalState(t, database, result.Receipt.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run = %+v, want completed with selected sentinel %s", run, sentinel)
	}
	gitCmd(t, gateDir, "merge-base", "--is-ancestor", sentinel, run.HeadSHA)
	var got *db.Repo
	select {
	case got = <-seen:
	case <-time.After(30 * time.Second):
		t.Fatal("selected integration never reached review")
	}
	if got.UpstreamURL != customerURL {
		t.Fatalf("integration destination = %q, want %q", got.UpstreamURL, customerURL)
	}
	if actual := gitOutput(t, gateDir, "config", "--get-regexp", "remote.origin."); actual != originConfig {
		t.Fatalf("origin configuration changed: %q -> %q", originConfig, actual)
	}
	if actual := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/"); actual != originRefs {
		t.Fatalf("origin refs changed: %q -> %q", originRefs, actual)
	}
	if actual := gitOutput(t, product, "for-each-ref", "--format=%(refname) %(objectname)"); actual != productRefs {
		t.Fatalf("origin's remote refs changed: %q -> %q", productRefs, actual)
	}
	if actual := gitOutput(t, custom, "rev-parse", "refs/heads/topic"); actual != run.HeadSHA {
		t.Fatalf("head was not delivered to the selected destination: %s", actual)
	}
	if actual := gitOutput(t, custom, "show", run.HeadSHA+":topic.txt"); actual != "topic change" {
		t.Fatalf("integration lost the submitted change: %q", actual)
	}
	log, err := os.ReadFile(forgeLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pr create", "--repo customer/project", "--base release/customer"} {
		if !strings.Contains(string(log), want) {
			t.Fatalf("missing %q from fresh-run forge calls:\n%s", want, log)
		}
	}
	if _, err := database.GetRun(run.ID); err != nil {
		t.Fatal(err)
	}
}

func commitIntegrationTopic(t *testing.T, workDir string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, "topic.txt"), []byte("topic change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "topic.txt")
	gitCmd(t, workDir, "commit", "-m", "change the customer topic")
	return gitOutput(t, workDir, "rev-parse", "HEAD")
}

var integrationCLI struct {
	sync.Once
	dir  string
	path string
	err  error
}

func integrationFakeForge(t *testing.T) ([]string, string) {
	t.Helper()
	integrationCLI.Do(func() {
		integrationCLI.dir, integrationCLI.err = os.MkdirTemp("", "nm1237-fake-")
		if integrationCLI.err != nil {
			return
		}
		integrationCLI.path = filepath.Join(integrationCLI.dir, "gh")
		if runtime.GOOS == "windows" {
			integrationCLI.path += ".exe"
		}
		command := exec.Command("go", "build", "-o", integrationCLI.path, "../pipeline/fakecli")
		if output, err := command.CombinedOutput(); err != nil {
			integrationCLI.err = fmt.Errorf("build non-race forge recorder: %w: %s", err, output)
		}
	})
	if integrationCLI.err != nil {
		t.Fatal(integrationCLI.err)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	log := filepath.Join(root, "forge.log")
	env := upsertEnv(os.Environ(), "PATH", integrationCLI.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range map[string]string{
		"FAKE_CLI_MODE": "gh", "FAKE_CLI_LOG": log, "FAKE_CLI_PR_URL": "",
		"FAKE_CLI_PR_BODY_FILE": filepath.Join(root, "body.md"), "FAKE_CLI_REAL_GIT": realGit,
	} {
		env = upsertEnv(env, key, value)
	}
	return env, log
}

type integrationEnvStep struct {
	step pipeline.Step
	env  []string
}

func (s *integrationEnvStep) Name() types.StepName { return s.step.Name() }
func (s *integrationEnvStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	sctx.Env = s.env
	return s.step.Execute(sctx)
}

// Model a completed review of the fixture's changed file, including the head
// certificate required by Push and by approval recovery after a restart.
type integrationReviewStep struct {
	seen chan<- *db.Repo
	park bool
}

func (*integrationReviewStep) Name() types.StepName { return types.StepReview }
func (s *integrationReviewStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if s.seen != nil {
		repo := *sctx.Repo
		s.seen <- &repo
	}
	return &pipeline.StepOutcome{
		NeedsApproval: s.park, Findings: `{"findings":[]}`, ReviewApprovedHeadSHA: head,
		ReviewedPaths: []string{"topic.txt"}, ReviewablePaths: []string{"topic.txt"},
	}, nil
}

func integrationRecoverySteps() []pipeline.Step {
	return []pipeline.Step{&steps.RebaseStep{}, &integrationReviewStep{park: true}, &integrationRecoveryStep{}}
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
	gitCmd(t, repo.WorkingPath, "push", "custom", "HEAD:refs/heads/main", "HEAD:refs/heads/release/customer")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "topic", head)
	initial := head
	head = commitIntegrationTopic(t, repo.WorkingPath)
	gitCmd(t, repo.WorkingPath, "push", "gate", "topic")
	gitCmd(t, gateDir, "update-ref", "refs/remotes/origin/main", initial)
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
	if run.Status != types.RunCompleted || run.Integration == nil || run.Integration.URL != custom {
		t.Fatalf("restarted run lost its destination: %+v", run)
	}
	gitCmd(t, gateDir, "merge-base", "--is-ancestor", sentinel, run.HeadSHA)
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
	// The server drains its connections before RunWithOptions returns.
	// Release this client's scanner before waiting for the child to exit.
	_ = client.Close()
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	process = nil
}

func TestIntegrationRemoteConcurrentRunsHaveSeparateDestinations(t *testing.T) {
	seen := make(chan *db.Repo, 2)
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&steps.RebaseStep{}, &integrationReviewStep{seen: seen}}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-concurrent")
	gateDir := p.RepoDir(repo.ID)
	if err := git.IsolateHooksPath(t.Context(), gateDir); err != nil {
		t.Fatal(err)
	}
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
		gitCmd(t, repo.WorkingPath, "push", name, "HEAD:refs/heads/main", "HEAD:refs/heads/release/customer")
		gitCmd(t, repo.WorkingPath, "push", "gate", head+":refs/heads/topic-"+name)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "--detach", head)
	head = commitIntegrationTopic(t, repo.WorkingPath)
	for i := range 2 {
		gitCmd(t, repo.WorkingPath, "push", "gate", head+":refs/heads/topic-customer"+fmt.Sprint(i))
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
		if run.Status != types.RunCompleted || run.Integration == nil || run.Integration.URL != remotes[result.i] {
			t.Fatalf("run used another destination: %+v", run)
		}
		gitCmd(t, gateDir, "merge-base", "--is-ancestor", sentinels[result.i], run.HeadSHA)
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

func TestIntegrationRemoteExistingPRRefusesDefaultPushAndFreshRun(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-pr-binding")
	gateDir := p.RepoDir(repo.ID)
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", gateDir)
	custom := filepath.Join(t.TempDir(), "custom.git")
	gitCmd(t, repo.WorkingPath, "clone", "--bare", gateDir, custom)
	gitCmd(t, custom, "symbolic-ref", "HEAD", "refs/heads/main")
	gitCmd(t, repo.WorkingPath, "remote", "add", "custom", custom)
	destination, err := integration.ResolveRequest(integration.WithRemote(t.Context(), "custom"), repo.WorkingPath)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := database.InsertRunWithDestination(repo.ID, "topic", head, head, nil, "", "", "", "", false, nil, db.RunPRText{}, nil, destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(previous.ID, "https://github.com/customer/project/pull/7"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(previous.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	newer, err := database.InsertRun(repo.ID, "topic", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "topic")
	followup := commitIntegrationTopic(t, repo.WorkingPath)
	gitCmd(t, repo.WorkingPath, "push", "gate", "topic")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, proof := range []bool{false, true} {
		params := &ipc.PushReceivedParams{Gate: gateDir, Ref: "refs/heads/topic", Old: head, New: followup, Intent: "update the existing PR"}
		if proof {
			params.LaunchNonce, params.ValidationGeneration = "followup", "one"
		}
		err := client.Call(ipc.MethodPushReceived, params, &ipc.PushReceivedResult{})
		if err == nil || !strings.Contains(err.Error(), "cannot inherit an existing PR from another integration remote") {
			t.Fatalf("push proof=%t did not refuse destination change: %v", proof, err)
		}
	}
	var launched ipc.StartFreshRunResult
	err = client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{
		RepoID: repo.ID, Branch: "topic", HeadSHA: followup, Intent: "update the existing PR",
		LaunchNonce: "fresh-followup", ValidationGeneration: "one",
	}, &launched)
	if err == nil || !strings.Contains(err.Error(), "cannot inherit an existing PR from another integration remote") {
		t.Fatalf("fresh launch did not refuse destination change: %v", err)
	}
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("refused submissions created runs: %+v, %v", runs, err)
	}
	err = client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{
		RepoID: repo.ID, Branch: "topic", HeadSHA: followup, Intent: "update the existing PR",
		LaunchNonce: "selected-followup", ValidationGeneration: "one", BaseRemote: "custom",
	}, &launched)
	if err != nil {
		t.Fatalf("same destination was refused: %v", err)
	}
	if run := waitForRunTerminalState(t, database, launched.Receipt.RunID); run.Status != types.RunCompleted {
		t.Fatalf("same-destination run = %+v", run)
	}
}

func TestIntegrationRemoteDefaultOriginAdmissionAndReceipt(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, head := setupTestGitRepo(t, p, database, "integration-origin-reattach")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	params := &ipc.StartFreshRunParams{
		RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "retain the origin default",
		LaunchNonce: "origin-default", ValidationGeneration: "one",
	}
	var first ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, params, &first); err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, database, first.Receipt.RunID)
	params.BaseRemote = "origin"
	var repeated ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, params, &repeated); err != nil {
		t.Fatalf("explicit origin could not reattach: %v", err)
	}
	if repeated.Receipt.RunID != first.Receipt.RunID {
		t.Fatalf("explicit origin created another run: %+v", repeated)
	}
	var claimed ipc.ClaimLaunchReceiptResult
	if err := client.Call(ipc.MethodClaimLaunchReceipt, &ipc.ClaimLaunchReceiptParams{
		RepoID: repo.ID, Branch: "main", SubmittedHeadSHA: head, LaunchNonce: params.LaunchNonce,
		ValidationGeneration: params.ValidationGeneration, IntentDigest: digestIntent(params.Intent), BaseRemote: "origin",
	}, &claimed); err != nil {
		t.Fatalf("explicit origin receipt disagreed with admission: %v", err)
	}
}

func TestIntegrationRemoteCleanupPreservesActiveInheritedNamespace(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "integration-ref-cleanup")
	gateDir := p.RepoDir(repo.ID)
	destination := &integration.Destination{
		Remote: "custom", URL: gateDir, TransportURL: gateDir, DefaultBranch: "main", Key: "abcdefghijklmnopqrstuvwx",
	}
	insert := func(branch string, pin *integration.Destination) *db.Run {
		t.Helper()
		run, err := database.InsertRunWithDestination(repo.ID, branch, head, head, nil, "", "", "", "", false, nil, db.RunPRText{}, nil, pin)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	source := insert("topic", destination)
	continuation := insert("topic", destination)
	otherPin := *destination
	otherPin.Key += "other"
	insert("other", &otherPin)
	for _, branch := range []string{"main", "release/customer", "topic"} {
		gitCmd(t, gateDir, "update-ref", destination.Ref(branch), head)
	}
	gitCmd(t, gateDir, "update-ref", otherPin.Ref("main"), head)
	wtDir := p.WorktreeDir(repo.ID, source.ID)
	if err := git.WorktreeAdd(t.Context(), gateDir, wtDir, head); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(source.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	manager.removeRunWorktree(repo.ID, source.ID, gateDir, wtDir, "test_finished")
	if got := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname)", destination.Ref("")); len(strings.Fields(got)) != 3 {
		t.Fatalf("active continuation lost its refs: %q", got)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("finished source worktree was not removed: %v", err)
	}
	if err := database.UpdateRunStatus(continuation.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	continuation, err = database.GetRun(continuation.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager.cleanupIntegrationRefs(t.Context(), continuation, gateDir)
	manager.cleanupIntegrationRefs(t.Context(), continuation, gateDir)
	if got := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname)", destination.Ref("")); got != "" {
		t.Fatalf("finished namespace retained refs: %s", got)
	}
	if got := gitOutput(t, gateDir, "rev-parse", otherPin.Ref("main")); got != head {
		t.Fatalf("cleanup removed another run's ref: %s", got)
	}
}

func TestIntegrationRemoteContinuationOwnsNewNamespace(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, _ := setupTestGitRepo(t, p, database, "integration-continuation")
	gitCmd(t, repo.WorkingPath, "remote", "add", "custom", p.RepoDir(repo.ID))
	gitCmd(t, p.RepoDir(repo.ID), "symbolic-ref", "HEAD", "refs/heads/main")
	source, err := integration.ResolveRequest(integration.WithRemote(t.Context(), "custom"), repo.WorkingPath)
	if err != nil {
		t.Fatal(err)
	}
	before := *source
	for range 2 {
		continuation, err := integration.ResolveRequest(integration.WithPinned(t.Context(), source), repo.WorkingPath)
		if err != nil {
			t.Fatal(err)
		}
		if continuation.Key == source.Key {
			t.Fatal("continuation shares refs with a run that may still be cleaning up")
		}
		if continuation.Remote != source.Remote || continuation.URL != source.URL || continuation.TransportURL != source.TransportURL || continuation.DefaultBranch != source.DefaultBranch || *source != before {
			t.Fatalf("continuation changed its destination: source=%+v continuation=%+v", source, continuation)
		}
	}
	gitCmd(t, repo.WorkingPath, "remote", "set-url", "custom", repo.WorkingPath)
	if _, err := integration.ResolveRequest(integration.WithPinned(t.Context(), source), repo.WorkingPath); err == nil {
		t.Fatal("continuation accepted a moved pinned destination")
	}
}

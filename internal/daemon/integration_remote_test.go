package daemon

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
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
	gitCmd(t, repo.WorkingPath, "remote", "add", "custom", custom)
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
	if got.UpstreamURL != custom {
		t.Fatalf("integration destination = %q, want %q", got.UpstreamURL, custom)
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

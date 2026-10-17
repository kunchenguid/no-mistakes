//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestProviderPluginJourney drives a full run against a host no built-in
// provider recognizes, with PR and CI handled by an operator-configured
// provider plugin: the global provider_plugins block is read by the real
// loader, the PR step opens the PR through the plugin, and the CI step reads
// checks, sees the PR merge at the run's head, and verifies the merged proof.
// The plugin is the reference fake (internal/scm/plugin/fakeplugin) behind
// the fakeagent binary, so the test exercises only the public CLI contract.
func TestProviderPluginJourney(t *testing.T) {
	const (
		pluginHost = "ssm.example.com"
		repoPath   = "team/repo"
		remoteURL  = "https://" + pluginHost + "/" + repoPath + ".git"
		branchName = "feature/provider-plugin-e2e"
	)
	h := NewHarness(t, SetupOpts{
		Agent: "claude",
		GlobalConfigExtra: "provider_plugins:\n" +
			"  ssm:\n" +
			"    command: " + fakeplugin.ExecutableName + "\n" +
			"    hosts: [" + pluginHost + "]\n" +
			"    timeout: 30s\n" +
			// This journey asserts what the plugin reports for a PR that merges
			// once its checks pass, so it opts into the watch that keeps
			// observing the PR after checks are green. The default would release
			// the run at the first green poll instead.
			"ci_monitor_until_merged: true\n",
	})

	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set plugin-host origin: %v\n%s", err, out)
	}

	stateDir := filepath.Dir(h.AgentLog)
	statePath := filepath.Join(stateDir, "provider-plugin-state.json")
	requestLog := filepath.Join(stateDir, "provider-plugin-requests.ndjson")
	if err := fakeplugin.Save(statePath, fakeplugin.State{
		Checks:              []fakeplugin.Check{{Name: "build", Bucket: "pass", State: "SUCCESS"}},
		MergeWhenChecksPass: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeplugin.EnvState, statePath)
	t.Setenv(fakeplugin.EnvLog, requestLog)

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branchName, "plugin.txt", "provider plugin e2e\n", "add provider plugin e2e file")
	h.PushToGate(branchName)

	// The CI step observes the merge on its second poll, 30s after the first.
	run := h.WaitForRun(branchName, 150*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	wantURL := "https://" + pluginHost + "/" + repoPath + "/pulls/1"
	if run.PRURL == nil || *run.PRURL != wantURL {
		t.Fatalf("PR URL = %v, want %s", run.PRURL, wantURL)
	}

	state, err := fakeplugin.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PRs) != 1 {
		t.Fatalf("plugin PRs = %+v", state.PRs)
	}
	pr := state.PRs[0]
	if pr.HeadBranch != branchName || pr.BaseBranch != "main" || pr.State != "merged" || pr.HeadSHA != run.HeadSHA {
		t.Fatalf("plugin PR = %+v, want %s -> main merged at %s", pr, branchName, run.HeadSHA)
	}
	if strings.TrimSpace(pr.Body) == "" || strings.TrimSpace(pr.Title) == "" {
		t.Fatalf("PR title/body not published: title=%q body=%q", pr.Title, pr.Body)
	}
	t.Logf("PR title: %s\nPR body:\n%s", pr.Title, pr.Body)

	calls, err := fakeplugin.ReadLog(requestLog)
	if err != nil {
		t.Fatalf("read plugin request log: %v", err)
	}
	ops := map[string]int{}
	for _, call := range calls {
		ops[call.Command]++
		if call.Flag("plugin") != "ssm" || call.Flag("host") != pluginHost || call.Flag("repo") != repoPath {
			t.Fatalf("context flags = %+v", call.Flags)
		}
		if strings.Contains(call.Flag("remote-url"), "@") {
			t.Fatalf("remote URL carries credentials: %q", call.Flag("remote-url"))
		}
	}
	for _, op := range []string{"status", "pr find", "pr create", "pr view", "pr checks", "pr merged"} {
		if ops[op] == 0 {
			t.Fatalf("pipeline never ran %q; subcommands: %v", op, ops)
		}
	}
	if ops["pr create"] != 1 {
		t.Fatalf("pr create ran %d times; subcommands: %v", ops["pr create"], ops)
	}
	t.Logf("plugin subcommands: %v\nPR: %s merged at %s", ops, *run.PRURL, pr.HeadSHA)
}

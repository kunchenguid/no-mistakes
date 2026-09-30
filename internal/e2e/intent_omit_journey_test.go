//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// omitIntentSlowBranch is a branch whose step prompts the scenario answers
// slowly, so the run stays observably active long enough for the reattach
// conflict check to be exercised against it.
const omitIntentSlowBranch = "feature/omit-reattach"

func writeOmitIntentScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omit-intent-scenario.yaml")
	content := `actions:
  - match: "branch: ` + omitIntentSlowBranch + `"
    delay_ms: 6000
    text: "slow clean step"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: omit intent journey"
      body: "## What Changed\n\n- Add the feature file.\n"
  - match: "Draft a pull request title and summary for the full branch delta."
    text: "PR drafted"
    structured:
      title: "feat: omit intent journey"
      body: "## What Changed\n\n- Add the feature file.\n"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write omit intent scenario: %v", err)
	}
	return path
}

// TestOmitIntentJourney drives the contributor-side Intent publication
// controls through the real binary, daemon, gate hook, and a fork-routed PR
// creation captured by the gh stub:
//
//   - a plain `axi run --intent` publishes the `## Intent` section (baseline);
//   - `axi run --intent --no-publish-intent` keeps the section and the intent
//     text out of the PR body while the review and PR-drafting prompts still
//     receive the full intent;
//   - a bare `rerun` after an omitting run inherits the omission;
//   - `intent.publish_intent: false` in global config omits the section for a
//     run started without the flag;
//   - `axi run --no-publish-intent` against an active run started without it
//     is refused rather than silently discarded.
func TestOmitIntentJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeOmitIntentScenario(t)})
	ctx := context.Background()

	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-omit-intent.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "parent-owner/no-mistakes")
	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init with fork URL: %v\n%s", err, out)
	}

	// prBodyFor returns the body of the LATEST PR create for the run's branch,
	// so a rerun's create supersedes the original run's.
	prBodyFor := func(run *ipc.RunInfo) string {
		t.Helper()
		var prBody string
		for _, inv := range readGHStubInvocations(t, ghLog) {
			if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" && strings.HasSuffix(inv.Head, ":"+run.Branch) {
				prBody = inv.Body
			}
		}
		if prBody == "" {
			t.Fatalf("no PR create for branch %s in gh log", run.Branch)
		}
		t.Logf("PR body for %s (run %s, omit_intent=%v):\n%s", run.Branch, run.ID, run.OmitIntent, prBody)
		return prBody
	}
	// promptsCarry proves the omission changes only publication for the
	// review prompt, which keeps the full intent, while the PR-drafting turn
	// is withheld the intent entirely so no paraphrase can reach the PR.
	promptsCarry := func(intent string) {
		t.Helper()
		invocations := h.AgentInvocations()
		const reviewMarker = "Review the code changes and return structured findings"
		const draftMarker = "Draft a pull request title and summary for the full branch delta."
		var reviewCarried, drafted bool
		for _, inv := range invocations {
			if strings.Contains(inv.Prompt, reviewMarker) && strings.Contains(inv.Prompt, intent) {
				reviewCarried = true
			}
			if strings.Contains(inv.Prompt, draftMarker) {
				drafted = true
				if strings.Contains(inv.Prompt, intent) {
					t.Errorf("PR-drafting prompt received the withheld intent %q", intent)
				}
			}
		}
		if !reviewCarried {
			t.Errorf("no review prompt carried the full intent %q", intent)
		}
		if !drafted {
			t.Errorf("no PR-drafting prompt ran")
		}
	}

	// Each completed fixture PR now really advances the parent target. Start
	// the next topic from that target, so this Intent journey does not also
	// introduce a rebase/custody transition in the contributor worktree.
	syncMain := func() {
		t.Helper()
		if out, err := h.runGit(ctx, h.WorkDir, "fetch", h.UpstreamDir, "refs/heads/main"); err != nil {
			t.Fatalf("fetch merged fixture main: %v\n%s", err, out)
		}
		if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
			t.Fatalf("checkout fixture main: %v\n%s", err, out)
		}
		if out, err := h.runGit(ctx, h.WorkDir, "merge", "--ff-only", "FETCH_HEAD"); err != nil {
			t.Fatalf("advance fixture main: %v\n%s", err, out)
		}
	}

	// Baseline: publication is the default.
	const publishedIntent = "publish this baseline intent on the PR body"
	h.CommitChange("feature/omit-baseline", "feature.txt", "baseline\n", "add baseline feature")
	baselineWT := h.AddWorktree("feature/omit-baseline")
	if out, err := h.RunInDir(baselineWT, "axi", "run", "--intent", publishedIntent); err != nil {
		t.Fatalf("baseline axi run: %v\n%s", err, out)
	}
	baseline := h.WaitForRun("feature/omit-baseline", 90*time.Second)
	if baseline.Status != types.RunCompleted {
		t.Fatalf("baseline run status = %s (error=%v)", baseline.Status, deref(baseline.Error))
	}
	if baseline.OmitIntent {
		t.Fatalf("baseline run recorded omit_intent without any control set")
	}
	baselineBody := prBodyFor(baseline)
	if !strings.Contains(baselineBody, "## Intent") || !strings.Contains(baselineBody, publishedIntent) {
		t.Fatalf("baseline PR body must publish the Intent section:\n%s", baselineBody)
	}

	// Per-run flag: section gone, prompts unchanged.
	const secretIntent = "SECRET-INTENT keep this contributor context off the public PR"
	syncMain()
	h.CommitChange("feature/omit-flag", "feature.txt", "flag\n", "add flag feature")
	flagWT := h.AddWorktree("feature/omit-flag")
	if out, err := h.RunInDir(flagWT, "axi", "run", "--intent", secretIntent, "--no-publish-intent"); err != nil {
		t.Fatalf("axi run --no-publish-intent: %v\n%s", err, out)
	}
	flagged := h.WaitForRun("feature/omit-flag", 90*time.Second)
	if flagged.Status != types.RunCompleted {
		t.Fatalf("flagged run status = %s (error=%v)", flagged.Status, deref(flagged.Error))
	}
	if !flagged.OmitIntent {
		t.Fatalf("flagged run did not record omit_intent")
	}
	flaggedBody := prBodyFor(flagged)
	if strings.Contains(flaggedBody, "## Intent") || strings.Contains(flaggedBody, "SECRET-INTENT") {
		t.Fatalf("--no-publish-intent PR body leaked the Intent section:\n%s", flaggedBody)
	}
	if !strings.Contains(flaggedBody, "## What Changed") {
		t.Fatalf("--no-publish-intent PR body lost the drafted narrative:\n%s", flaggedBody)
	}
	promptsCarry(secretIntent)

	// A bare rerun inherits the omission; the flag is tighten-only so nothing
	// on the rerun surface can restore the section. Give it a fresh delta so
	// it must draft a new PR, rather than complete with an already merged diff.
	if out, err := h.runGit(ctx, flagWT, "fetch", h.UpstreamDir, "refs/heads/main"); err != nil {
		t.Fatalf("fetch merged target for rerun: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, flagWT, "merge", "--ff-only", "FETCH_HEAD"); err != nil {
		t.Fatalf("advance rerun branch to merged target: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(flagWT, "rerun.txt"), []byte("fresh rerun delta\n"), 0o644); err != nil {
		t.Fatalf("write rerun delta: %v", err)
	}
	if out, err := h.runGit(ctx, flagWT, "add", "rerun.txt"); err != nil {
		t.Fatalf("stage rerun delta: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, flagWT, "commit", "-m", "add rerun delta"); err != nil {
		t.Fatalf("commit rerun delta: %v\n%s", err, out)
	}
	// Seed the local gate ref without invoking its push hook; bare rerun must
	// select this clean caller head while inheriting the previous run's flag.
	gateDir := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
	if out, err := h.runGit(ctx, gateDir, "fetch", flagWT, "refs/heads/"+flagged.Branch+":refs/heads/"+flagged.Branch); err != nil {
		t.Fatalf("seed fresh rerun gate head: %v\n%s", err, out)
	}
	beforeRerun := len(readGHStubInvocations(t, ghLog))
	rerun := runCLIAndWait(t, h, flagWT, "feature/omit-flag", "rerun")
	if rerun.ID == flagged.ID || !rerun.OmitIntent {
		t.Fatalf("rerun %s omit_intent=%v, want a new run inheriting omission from %s", rerun.ID, rerun.OmitIntent, flagged.ID)
	}
	var rerunBody string
	var rerunCreates int
	for _, inv := range readGHStubInvocations(t, ghLog)[beforeRerun:] {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" && strings.HasSuffix(inv.Head, ":"+rerun.Branch) {
			rerunCreates++
			rerunBody = inv.Body
		}
	}
	if rerunCreates != 1 || !strings.Contains(rerunBody, "## What Changed") {
		t.Fatalf("rerun made %d new PR creates, want one with a drafted body:\n%s", rerunCreates, rerunBody)
	}
	if strings.Contains(rerunBody, "## Intent") || strings.Contains(rerunBody, "SECRET-INTENT") {
		t.Fatalf("rerun PR body re-published the Intent section:\n%s", rerunBody)
	}

	// Global default: intent.publish_intent: false omits for a run started
	// without the flag. The daemon reads global config at run start.
	configPath := filepath.Join(h.NMHome, "config.yaml")
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	if err := os.WriteFile(configPath, append(append([]byte{}, original...), []byte("intent:\n  publish_intent: false\n")...), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	const globalIntent = "GLOBAL-DEFAULT intent that the operator keeps private"
	syncMain()
	h.CommitChange("feature/omit-global", "feature.txt", "global\n", "add global feature")
	globalWT := h.AddWorktree("feature/omit-global")
	if out, err := h.RunInDir(globalWT, "axi", "run", "--intent", globalIntent); err != nil {
		t.Fatalf("axi run under global publish_intent: false: %v\n%s", err, out)
	}
	global := h.WaitForRun("feature/omit-global", 90*time.Second)
	if global.Status != types.RunCompleted {
		t.Fatalf("global-default run status = %s (error=%v)", global.Status, deref(global.Error))
	}
	if !global.OmitIntent {
		t.Fatalf("global publish_intent: false did not stamp omit_intent on the run")
	}
	globalBody := prBodyFor(global)
	if strings.Contains(globalBody, "## Intent") || strings.Contains(globalBody, "GLOBAL-DEFAULT") {
		t.Fatalf("global publish_intent: false PR body leaked the Intent section:\n%s", globalBody)
	}
	promptsCarry(globalIntent)
	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatalf("restore global config: %v", err)
	}

	// Adversarial reattach: an active run started WITHOUT the flag must not be
	// silently adopted by a caller that asked for omission.
	syncMain()
	h.CommitChange(omitIntentSlowBranch, "feature.txt", "reattach\n", "add reattach feature")
	reattachWT := h.AddWorktree(omitIntentSlowBranch)
	h.PushToGate(omitIntentSlowBranch)
	if running := h.WaitForRunRunning(omitIntentSlowBranch, 60*time.Second); running.OmitIntent {
		t.Fatalf("plain gate push recorded omit_intent: %+v", running)
	}
	out, err := h.RunInDir(reattachWT, "axi", "run", "--intent", "reattach with omission", "--no-publish-intent")
	if err == nil || !strings.Contains(out, "without --no-publish-intent") || !strings.Contains(out, "Omit --no-publish-intent to reattach") {
		t.Fatalf("axi run --no-publish-intent against a publishing active run should be refused, err=%v output:\n%s", err, out)
	}
	t.Logf("refused reattach:\n%s", strings.TrimSpace(out))
	reattach := h.WaitForRun(omitIntentSlowBranch, 120*time.Second)
	if reattach.Status != types.RunCompleted || reattach.OmitIntent {
		t.Fatalf("active run after the refused reattach: status=%s omit_intent=%v (error=%v)", reattach.Status, reattach.OmitIntent, deref(reattach.Error))
	}
	runsOnBranch := 0
	for _, r := range h.Runs() {
		if r.Branch == omitIntentSlowBranch {
			runsOnBranch++
		}
	}
	if runsOnBranch != 1 {
		t.Fatalf("refused reattach must not start a second run on %s, got %d runs", omitIntentSlowBranch, runsOnBranch)
	}
}

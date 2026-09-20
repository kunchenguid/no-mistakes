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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys drive rebase.strategy through the real binary: a real gate, a
// real daemon, a real base branch that moved, and a real remote. The unit tests
// in internal/pipeline/steps/rebase_merge_test.go drive the step in isolation;
// what only a journey can show is the shape the pipeline actually PUBLISHES,
// and that a remote which refuses to have its branch rewritten accepts it.

// harnessRepoConfigWithExtra rebuilds the harness's default-branch
// .no-mistakes.yaml with extra appended. rebase.strategy is trusted-only, so a
// journey has to set it the way a maintainer does: committed on the default
// branch, never on the pushed branch.
func harnessRepoConfigWithExtra(extra string) string {
	return "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\n" + extra
}

// commitTrustedRepoConfig commits a .no-mistakes.yaml on main and pushes it to
// origin, which is where the daemon reads the trusted copy from.
func commitTrustedRepoConfig(t *testing.T, h *Harness, extra string) {
	t.Helper()
	h.CommitChange("main", ".no-mistakes.yaml", harnessRepoConfigWithExtra(extra), "set trusted repo config")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted repo config: %v\n%s", err, out)
	}
}

// initFromOwnWorktree runs `no-mistakes init` off a branch of its own so the
// journey's feature branch is never the one init was standing on.
func initFromOwnWorktree(t *testing.T, h *Harness, name string) {
	t.Helper()
	h.CommitChange(name, name+".txt", "init\n", "seed "+name)
	wt := h.AddWorktree(name)
	if out, err := h.RunInDir(wt, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
}

// advanceMain moves the default branch under the gated branch and publishes it,
// which is the only condition under which the rebase step integrates anything.
func advanceMain(t *testing.T, h *Harness, path, content, message string) string {
	t.Helper()
	sha := h.CommitChange("main", path, content, message)
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("advance origin/main: %v\n%s", err, out)
	}
	return sha
}

func gitIn(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func isAncestorIn(t *testing.T, h *Harness, dir, ancestor, descendant string) bool {
	t.Helper()
	_, err := h.runGit(context.Background(), dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// parentsOf returns a commit's parent SHAs in order, so first-parent identity
// can be asserted rather than inferred.
func parentsOf(t *testing.T, h *Harness, dir, rev string) []string {
	t.Helper()
	fields := strings.Fields(gitIn(t, h, dir, "rev-list", "--parents", "-n", "1", rev))
	if len(fields) == 0 {
		t.Fatalf("no commit for %s in %s", rev, dir)
	}
	return fields[1:]
}

// mergeOfOnFirstParentChain finds the merge commit that joined wantFirst and
// wantSecond on rev's first-parent chain. Walking the chain rather than
// inspecting rev itself keeps the assertion exact while still tolerating a
// pipeline commit landing on top of the integration.
func mergeOfOnFirstParentChain(t *testing.T, h *Harness, dir, rev, wantFirst, wantSecond string) string {
	t.Helper()
	for _, commit := range strings.Fields(gitIn(t, h, dir, "rev-list", "--first-parent", rev)) {
		parents := parentsOf(t, h, dir, commit)
		if len(parents) == 2 && parents[0] == wantFirst && parents[1] == wantSecond {
			return commit
		}
	}
	return ""
}

// TestRebaseMergeStrategyPublishesAMergeOfTheReviewedHeadJourney is the opt-in
// contract end to end. A repository whose default branch sets
// rebase.strategy: merge pushes a branch whose base has moved, and the head the
// pipeline publishes must be a merge commit whose FIRST parent is the head the
// operator submitted - the property the CI step's continuity rule and the
// attestation's head binding both read.
//
// The remote is configured to refuse any non-fast-forward update to that branch
// before the run, which is what makes the publication claim testable rather
// than asserted: under the default rebase strategy the same run would have to
// rewrite the branch the remote already carries.
func TestRebaseMergeStrategyPublishesAMergeOfTheReviewedHeadJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	commitTrustedRepoConfig(t, h, "rebase:\n  strategy: merge\n")
	initFromOwnWorktree(t, h, "init-merge-strategy")

	const branch = "feature/merge-strategy"
	submitted := h.CommitChange(branch, "feature.txt", "feature work\n", "add feature work")

	// Publish the branch first, so the pipeline's own publication is an UPDATE
	// to an existing remote branch - the case an open PR is in - and forbid the
	// remote from accepting a rewrite of it.
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", branch); err != nil {
		t.Fatalf("pre-publish %s: %v\n%s", branch, err, out)
	}
	gitIn(t, h, h.UpstreamDir, "config", "receive.denyNonFastForwards", "true")

	advanceMain(t, h, "main-advance.txt", "main advanced\n", "advance main under the branch")
	h.Checkout(branch)
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("merge-strategy run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	published := h.UpstreamBranchSHA(branch)
	mainSHA := h.UpstreamBranchSHA("main")
	if published != run.HeadSHA {
		t.Fatalf("published head %s != run head %s", published, run.HeadSHA)
	}
	if published == submitted {
		t.Fatalf("nothing was integrated: published head still the submitted head %s", submitted)
	}
	// The reviewed head survives, which is what the rebase shape destroys.
	if !isAncestorIn(t, h, h.UpstreamDir, submitted, published) {
		t.Fatalf("reviewed head %s is not an ancestor of the published head %s", submitted, published)
	}
	if !isAncestorIn(t, h, h.UpstreamDir, mainSHA, published) {
		t.Fatalf("moved base %s is not an ancestor of the published head %s", mainSHA, published)
	}
	mergeCommit := mergeOfOnFirstParentChain(t, h, h.UpstreamDir, published, submitted, mainSHA)
	if mergeCommit == "" {
		t.Fatalf("published head %s carries no merge commit with parents [%s, %s]:\n%s",
			published, submitted, mainSHA,
			gitIn(t, h, h.UpstreamDir, "log", "--format=%h %p %s", "-8", published))
	}
	t.Logf("published history on the remote (%s refuses non-fast-forward updates):\n%s",
		branch, gitIn(t, h, h.UpstreamDir, "log", "--graph", "--format=%h %p %d %s", "-6", published))

	// Both integrated trees are present in the worktree the merge produced.
	files := gitIn(t, h, h.UpstreamDir, "ls-tree", "--name-only", "-r", published)
	for _, want := range []string{"feature.txt", "main-advance.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("published tree is missing %s:\n%s", want, files)
		}
	}
}

// TestRebaseMergeStrategyPushedBranchCannotSelfSelectJourney is the trust
// boundary, driven rather than reasoned about: the default branch says nothing
// about rebase.strategy, and the pushed branch declares merge in its own
// .no-mistakes.yaml. The run must integrate by rebasing anyway, so a
// contributor cannot opt their own branch into a different evidence shape than
// the maintainer chose.
func TestRebaseMergeStrategyPushedBranchCannotSelfSelectJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	initFromOwnWorktree(t, h, "init-self-select")

	const branch = "feature/self-select-merge"
	h.CommitChange(branch, "feature.txt", "feature work\n", "add feature work")
	submitted := h.CommitChange(branch, ".no-mistakes.yaml",
		harnessRepoConfigWithExtra("rebase:\n  strategy: merge\n"),
		"self-select the merge strategy from the pushed branch")

	advanceMain(t, h, "main-advance.txt", "main advanced\n", "advance main under the branch")
	h.Checkout(branch)
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("self-select run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	published := h.UpstreamBranchSHA(branch)
	mainSHA := h.UpstreamBranchSHA("main")
	if published == submitted {
		t.Fatalf("nothing was integrated: published head still the submitted head %s", submitted)
	}
	if isAncestorIn(t, h, h.UpstreamDir, submitted, published) {
		t.Fatalf("pushed branch selected the merge shape: submitted head %s survived in %s", submitted, published)
	}
	if got := gitIn(t, h, h.UpstreamDir, "merge-base", published, mainSHA); got != mainSHA {
		t.Fatalf("published head %s is not a replay onto the moved base: merge-base %s, want %s", published, got, mainSHA)
	}
}

// TestRebaseMergeStrategyUnparseableTrustedValueFailsRunClosedJourney is the
// typo case. The maintainer's default-branch config says "merges"; the pushed
// branch's own copy is valid, so nothing but the trusted copy is malformed. The
// run must refuse rather than degrade to an absent trusted config and quietly
// keep rewriting history the maintainer asked it to stop rewriting.
func TestRebaseMergeStrategyUnparseableTrustedValueFailsRunClosedJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	commitTrustedRepoConfig(t, h, "rebase:\n  strategy: merges\n")
	initFromOwnWorktree(t, h, "init-bad-strategy")

	const branch = "feature/bad-strategy"
	// The pushed branch itself carries a VALID config, so the only malformed
	// copy in play is the trusted one.
	h.CommitChange(branch, ".no-mistakes.yaml", harnessRepoConfigWithExtra(""), "restore a valid pushed config")
	submitted := h.CommitChange(branch, "feature.txt", "feature work\n", "add feature work")

	advanceMain(t, h, "main-advance.txt", "main advanced\n", "advance main under the branch")
	h.Checkout(branch)
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status == types.RunCompleted {
		t.Fatalf("run completed under an unparseable trusted rebase.strategy (head %s)", run.HeadSHA)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "unparseable") {
		t.Fatalf("run error = %v, want it to name the unparseable trusted config", deref(run.Error))
	}
	if _, err := h.runGit(context.Background(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("a run that failed closed still published %s", branch)
	}
	if got := h.WorktreeRefSHA(branch); got != submitted {
		t.Fatalf("local branch moved after a fail-closed run: %s, want %s", got, submitted)
	}
}

// mergeConflictScenario is the clean default scenario plus one conflict-
// resolution action. resolution names how the agent ends the conflict, which is
// the whole variable the merge-shape guard judges.
func mergeConflictScenario(t *testing.T, resolution []string, stage bool) string {
	t.Helper()
	action := `actions:
  - match: "Resolve git merge conflicts"
    text: "resolved the conflict"
    edits:
      - path: "shared.txt"
        new: "base\nfeature change\nmain change\n"
`
	if stage {
		action += "    stage: [\"shared.txt\"]\n"
	}
	if len(resolution) > 0 {
		action += "    git:\n"
		for _, cmd := range resolution {
			action += "      - [" + cmd + "]\n"
		}
	}
	action += `    structured:
      summary: "kept both sides"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risk"
      risk_scope: source-or-external
      tested: ["fakeagent: focused verification"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: merge strategy conflict"
      body: "merge strategy conflict journey"
`
	path := filepath.Join(t.TempDir(), "merge-conflict-scenario.yaml")
	if err := os.WriteFile(path, []byte(action), 0o644); err != nil {
		t.Fatalf("write merge conflict scenario: %v", err)
	}
	return path
}

// setupMergeConflict puts the same file on a collision course between the
// branch and the moved base, and returns the submitted head.
func setupMergeConflict(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	h.CommitChange("main", "shared.txt", "base\n", "add the shared file")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push shared base: %v\n%s", err, out)
	}
	submitted := h.CommitChange(branch, "shared.txt", "base\nfeature change\n", "change the shared file on the branch")
	advanceMain(t, h, "shared.txt", "base\nmain change\n", "change the shared file on main")
	h.Checkout(branch)
	return submitted
}

// waitForRebaseConflictGate drives the run to the rebase step's conflict gate
// and returns it. Under merge strategy the finding has to describe what the
// step is actually doing, so the wording is asserted here rather than left to a
// prompt-text check.
func waitForRebaseConflictGate(t *testing.T, h *Harness, branch string) *ipc.RunInfo {
	t.Helper()
	run := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
	step, ok := findStep(run.Steps, types.StepRebase)
	if !ok || step.FindingsJSON == nil {
		t.Fatalf("rebase conflict gate recorded no findings: %+v", run.Steps)
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse rebase conflict findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "merging origin/main") {
		t.Fatalf("conflict finding does not describe a merge: %+v", findings.Items)
	}
	return run
}

// TestRebaseMergeStrategyAgentResolvedConflictPublishesAMergeJourney is the
// conflict path of the opt-in: the agent resolves the markers additively and
// concludes with a commit, and the published head is the two-parent commit the
// resolution can be audited against afterwards.
func TestRebaseMergeStrategyAgentResolvedConflictPublishesAMergeJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: mergeConflictScenario(t, []string{`"commit", "--no-edit"`}, true),
	})
	commitTrustedRepoConfig(t, h, "rebase:\n  strategy: merge\n")
	initFromOwnWorktree(t, h, "init-merge-conflict")

	const branch = "feature/merge-conflict"
	submitted := setupMergeConflict(t, h, branch)
	h.PushToGate(branch)

	run := waitForRebaseConflictGate(t, h, branch)
	h.Respond(run.ID, types.StepRebase, types.ActionFix)

	completed := h.WaitForRun(branch, 120*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("resolved-conflict run did not complete: status=%s error=%v", completed.Status, deref(completed.Error))
	}

	published := h.UpstreamBranchSHA(branch)
	mainSHA := h.UpstreamBranchSHA("main")
	if !isAncestorIn(t, h, h.UpstreamDir, submitted, published) {
		t.Fatalf("reviewed head %s is not an ancestor of the published head %s", submitted, published)
	}
	if mergeOfOnFirstParentChain(t, h, h.UpstreamDir, published, submitted, mainSHA) == "" {
		t.Fatalf("published head %s carries no merge of [%s, %s]:\n%s", published, submitted, mainSHA,
			gitIn(t, h, h.UpstreamDir, "log", "--format=%h %p %s", "-8", published))
	}
	// The resolution is auditable from outside the pipeline precisely because
	// both sides are still reachable: neither side's line was dropped.
	resolved := gitIn(t, h, h.UpstreamDir, "show", published+":shared.txt")
	for _, want := range []string{"feature change", "main change"} {
		if !strings.Contains(resolved, want) {
			t.Errorf("resolution dropped %q:\n%s", want, resolved)
		}
	}
}

// TestRebaseMergeStrategyRejectsAResolutionThatDidNotMergeJourney is the
// adversarial one. The agent ends the conflict without merging - it abandons
// the merge and resets the branch onto the moved base, which leaves a head that
// contains the target and passes a naive ancestry check but has dropped the
// reviewed head. The run must fail rather than carry that head forward, and the
// worktree must be left back on the reviewed head rather than on the rejected
// one.
func TestRebaseMergeStrategyRejectsAResolutionThatDidNotMergeJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: mergeConflictScenario(t, []string{`"merge", "--abort"`, `"reset", "--hard", "origin/main"`}, false),
	})
	commitTrustedRepoConfig(t, h, "rebase:\n  strategy: merge\n")
	initFromOwnWorktree(t, h, "init-merge-rejected")

	const branch = "feature/merge-rejected"
	submitted := setupMergeConflict(t, h, branch)
	h.PushToGate(branch)

	run := waitForRebaseConflictGate(t, h, branch)
	h.Respond(run.ID, types.StepRebase, types.ActionFix)

	failed := h.WaitForRun(branch, 120*time.Second)
	if failed.Status != types.RunFailed {
		t.Fatalf("run status = %s (error=%v), want failed for a resolution that did not merge", failed.Status, deref(failed.Error))
	}
	if failed.Error == nil || !strings.Contains(*failed.Error, "did not merge origin/main into the branch") {
		t.Fatalf("run error = %v, want it to name the unproven merge", deref(failed.Error))
	}
	if _, err := h.runGit(context.Background(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("a rejected resolution was still published to the remote")
	}

	if failed.HeadSHA != submitted {
		t.Fatalf("run recorded head %s, want the reviewed head %s", failed.HeadSHA, submitted)
	}

	// The operator-facing consequence of restoring the reviewed head before
	// returning the rejection: custody comes back at the head they submitted.
	// Left on the rejected head, the same commands report the branch as
	// pipeline-owned at a head no step ever validated, and demand a recovery.
	operator := h.AddWorktree(branch)
	statusOut, err := h.RunInDir(operator, "axi", "status")
	if err != nil {
		t.Fatalf("axi status after the rejection: %v\n%s", err, statusOut)
	}
	checkOut, _ := h.RunInDir(operator, "axi", "sync", "--check")
	t.Logf("axi status after the rejection:\n%s\naxi sync --check:\n%s", statusOut, checkOut)
	for _, want := range []string{"state: user_owned", "safety: user_owned", submitted} {
		if !strings.Contains(statusOut, want) {
			t.Errorf("axi status does not report %q after the rejection:\n%s", want, statusOut)
		}
	}
	if strings.Contains(statusOut, "pipeline_owned") {
		t.Errorf("axi status leaves the branch pipeline-owned after a rejected merge:\n%s", statusOut)
	}
}

// TestRebaseMergeStrategyRejectsAnUnconcludedMergeJourney covers the other way
// an agent can leave the conflict: it resolves and stages the files but never
// commits, so the merge is still in progress. Left unguarded the run would
// carry the reviewed head forward as if the base had never moved.
func TestRebaseMergeStrategyRejectsAnUnconcludedMergeJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: mergeConflictScenario(t, nil, true),
	})
	commitTrustedRepoConfig(t, h, "rebase:\n  strategy: merge\n")
	initFromOwnWorktree(t, h, "init-merge-unconcluded")

	const branch = "feature/merge-unconcluded"
	submitted := setupMergeConflict(t, h, branch)
	h.PushToGate(branch)

	run := waitForRebaseConflictGate(t, h, branch)
	h.Respond(run.ID, types.StepRebase, types.ActionFix)

	failed := h.WaitForRun(branch, 120*time.Second)
	if failed.Status != types.RunFailed {
		t.Fatalf("run status = %s (error=%v), want failed for an unconcluded merge", failed.Status, deref(failed.Error))
	}
	if failed.Error == nil || !strings.Contains(*failed.Error, "did not complete the merge") {
		t.Fatalf("run error = %v, want it to name the unconcluded merge", deref(failed.Error))
	}
	if _, err := h.runGit(context.Background(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("an unconcluded merge was still published to the remote")
	}
	if failed.HeadSHA != submitted {
		t.Fatalf("run recorded head %s, want the reviewed head %s", failed.HeadSHA, submitted)
	}
}

// TestRebaseMergeStrategyGlobalDefaultAndTrustedOverrideJourney drives the
// precedence the option promises an operator: a machine-wide default applies to
// a repository that says nothing, and a repository that does say something wins
// over it. Both halves run against the same daemon, so the only variable
// between them is the trusted .no-mistakes.yaml.
func TestRebaseMergeStrategyGlobalDefaultAndTrustedOverrideJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: "rebase:\n  strategy: merge"})
	initFromOwnWorktree(t, h, "init-global-default")

	// Half one: the repository is silent, so the operator's default decides.
	const inherits = "feature/inherits-global-merge"
	inheritsSubmitted := h.CommitChange(inherits, "inherits.txt", "inherits\n", "add work that inherits the global default")
	advanceMain(t, h, "advance-one.txt", "one\n", "advance main under the inheriting branch")
	h.Checkout(inherits)
	h.PushToGate(inherits)
	run := h.WaitForRun(inherits, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("global-default run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	published := h.UpstreamBranchSHA(inherits)
	if !isAncestorIn(t, h, h.UpstreamDir, inheritsSubmitted, published) {
		t.Fatalf("global rebase.strategy: merge did not apply: %s is not an ancestor of %s", inheritsSubmitted, published)
	}
	if mergeOfOnFirstParentChain(t, h, h.UpstreamDir, published, inheritsSubmitted, h.UpstreamBranchSHA("main")) == "" {
		t.Fatalf("global-default run published no merge commit:\n%s",
			gitIn(t, h, h.UpstreamDir, "log", "--format=%h %p %s", "-6", published))
	}

	// Half two: the maintainer's trusted config picks rebase explicitly, which
	// has to beat the operator's global merge.
	commitTrustedRepoConfig(t, h, "rebase:\n  strategy: rebase\n")
	const overrides = "feature/trusted-override-rebase"
	overridesSubmitted := h.CommitChange(overrides, "overrides.txt", "overrides\n", "add work under the trusted override")
	advanceMain(t, h, "advance-two.txt", "two\n", "advance main under the overriding branch")
	h.Checkout(overrides)
	h.PushToGate(overrides)
	overrideRun := h.WaitForRun(overrides, 90*time.Second)
	if overrideRun.Status != types.RunCompleted {
		t.Fatalf("trusted-override run did not complete: status=%s error=%v", overrideRun.Status, deref(overrideRun.Error))
	}
	overridePublished := h.UpstreamBranchSHA(overrides)
	if overridePublished == overridesSubmitted {
		t.Fatalf("nothing was integrated under the trusted override: still %s", overridesSubmitted)
	}
	if isAncestorIn(t, h, h.UpstreamDir, overridesSubmitted, overridePublished) {
		t.Fatalf("trusted rebase.strategy: rebase did not beat the global merge: %s survived in %s", overridesSubmitted, overridePublished)
	}
}

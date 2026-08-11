//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"
)

// TestAxiRemoteRewrittenRecoveryJourney reproduces the operator-facing dead end
// a force-rewritten remote used to leave behind, with the real binary: a run
// completes and publishes its pipeline head, then something outside this
// pipeline force-updates the remote branch. The persisted push binding now
// names a commit the remote no longer has.
//
// The journey proves the state is no longer a dead end that quietly carries a
// stale pushed head forward: `axi sync --check` reports blocked_remote_rewritten
// with a next_action that actually runs, `sync --recover` corrects only the
// recorded binding after anchoring the superseded pipeline head, and no
// worktree, branch ref, or gate ref is touched. Afterwards the operator is back
// on the ordinary guarded path against the real remote head.
func TestAxiRemoteRewrittenRecoveryJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-rewritten", "seed.txt", "seed\n", "seed rewritten init")
	initWorktree := h.AddWorktree("init-rewritten")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/remote-rewritten-journey"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree(branch)

	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature and publish it")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("review fix: %v\n%s", err, fixOut)
	}
	doneOut, err := h.RunInDir(operator, "axi", "respond", "--action", "approve")
	if err != nil || !strings.Contains(doneOut, "outcome: passed") {
		t.Fatalf("approve fix review: %v\n%s", err, doneOut)
	}
	run := h.WaitForRun(branch, 30*time.Second)
	pipelineHead := h.UpstreamBranchSHA(branch)
	if pipelineHead == submitted {
		t.Fatal("pipeline did not publish a fix commit")
	}

	// Something outside this pipeline force-updates the remote branch: a
	// colleague, another tool, or a hand `git push --force`. The published
	// pipeline head is replaced by a sibling history built on the submitted
	// commit, so it is not an ancestor of the new remote head.
	ctx := context.Background()
	rewriterParent := t.TempDir()
	rewriter := filepath.Join(rewriterParent, "rewriter")
	if out, err := h.runGit(ctx, rewriterParent, "clone", h.UpstreamDir, rewriter); err != nil {
		t.Fatalf("clone upstream: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"config", "user.email", "outsider@example.com"},
		{"config", "user.name", "Outsider"},
		{"checkout", "-B", branch, submitted},
	} {
		if out, err := h.runGit(ctx, rewriter, args...); err != nil {
			t.Fatalf("git %v in rewriter: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(rewriter, "outside.txt"), []byte("rewritten outside the pipeline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "outside.txt"},
		{"commit", "-m", "force-pushed correction from outside the pipeline"},
		{"push", "--force", "origin", branch},
	} {
		if out, err := h.runGit(ctx, rewriter, args...); err != nil {
			t.Fatalf("git %v in rewriter: %v\n%s", args, err, out)
		}
	}
	rewritten := h.UpstreamBranchSHA(branch)
	if rewritten == pipelineHead {
		t.Fatal("out-of-band force push did not rewrite the remote branch")
	}
	if out, err := h.runGit(ctx, rewriter, "merge-base", "--is-ancestor", pipelineHead, rewritten); err == nil {
		t.Fatalf("rewritten remote still contains the pipeline head, so this is an advance, not a rewrite:\n%s", out)
	}

	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	gateHeadBytes, err := h.runGit(ctx, gateDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("gate head: %v\n%s", err, gateHeadBytes)
	}
	gateHead := strings.TrimSpace(string(gateHeadBytes))

	// The agent surface must name the hazard and offer a route out of it.
	checkOut, checkErr := h.RunInDir(operator, "axi", "sync", "--check")
	if checkErr == nil {
		t.Fatalf("rewritten-remote sync --check should exit non-zero:\n%s", checkOut)
	}
	for _, want := range []string{
		"state: remote_rewritten",
		"safety: blocked_remote_rewritten",
		"code: recover_remote_rewritten",
		"command: no-mistakes axi sync --recover",
		"no longer equals the persisted pipeline push binding",
	} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("rewritten-remote check missing %q:\n%s", want, checkOut)
		}
	}

	// The human surface takes the offered correction and says exactly what it
	// did: the binding only, never a custody return.
	recoverOut, err := h.RunInDir(operator, "sync", "--recover", "--yes")
	if err != nil {
		t.Fatalf("guarded binding correction: %v\n%s", err, recoverOut)
	}
	for _, want := range []string{
		"Push binding corrected to the verified remote head " + rewritten,
		"no worktree, branch, or gate ref was changed",
	} {
		if !strings.Contains(recoverOut, want) {
			t.Errorf("binding-correction output missing %q:\n%s", want, recoverOut)
		}
	}
	if strings.Contains(recoverOut, "Custody returned") {
		t.Errorf("binding correction reported a custody return:\n%s", recoverOut)
	}

	// Nothing but the recorded binding moved.
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Errorf("operator branch moved to %s, want unchanged %s", got, submitted)
	}
	if got := h.UpstreamBranchSHA(branch); got != rewritten {
		t.Errorf("remote branch moved to %s, want the untouched rewritten head %s", got, rewritten)
	}
	afterGateBytes, err := h.runGit(ctx, gateDir, "rev-parse", "refs/heads/"+branch)
	if err != nil || strings.TrimSpace(string(afterGateBytes)) != gateHead {
		t.Errorf("gate head moved to %s (err %v), want unchanged %s", strings.TrimSpace(string(afterGateBytes)), err, gateHead)
	}

	// The superseded pipeline head stays reachable through the tool.
	anchorRef := "refs/no-mistakes/recover/" + run.ID
	anchorBytes, err := h.runGit(ctx, operator, "rev-parse", anchorRef)
	if err != nil || strings.TrimSpace(string(anchorBytes)) != pipelineHead {
		t.Fatalf("anchor %s = %s (err %v), want superseded pipeline head %s", anchorRef, strings.TrimSpace(string(anchorBytes)), err, pipelineHead)
	}

	// The operator is back on the ordinary guarded path: the corrected binding
	// names the real remote head, and the local branch is simply behind it.
	afterOut, err := h.RunInDir(operator, "axi", "sync", "--check")
	if err != nil {
		t.Fatalf("post-correction check should no longer be blocked: %v\n%s", err, afterOut)
	}
	// Decode the document rather than matching a raw line: TOON quotes a
	// scalar that begins with a digit-ambiguous prefix, so a SHA starting with
	// "0" renders as `pushed_head: "0035..."` and a raw substring would flake
	// on roughly one commit in sixteen.
	var afterDoc struct {
		BranchSync struct {
			State    string `toon:"state"`
			Pipeline struct {
				PushedHead string `toon:"pushed_head"`
			} `toon:"pipeline"`
		} `toon:"branch_sync"`
	}
	if err := toon.UnmarshalString(afterOut, &afterDoc); err != nil {
		t.Fatalf("decode post-correction check TOON: %v\n%s", err, afterOut)
	}
	if got := afterDoc.BranchSync.State; got != "behind" {
		t.Errorf("post-correction state = %q, want %q:\n%s", got, "behind", afterOut)
	}
	if got := afterDoc.BranchSync.Pipeline.PushedHead; got != rewritten {
		t.Errorf("post-correction pushed head = %q, want the verified remote head %q:\n%s", got, rewritten, afterOut)
	}
	if strings.Contains(afterOut, "remote_rewritten") {
		t.Errorf("post-correction check still reports a rewritten remote:\n%s", afterOut)
	}

	syncOut, err := h.RunInDir(operator, "axi", "sync")
	if err != nil {
		t.Fatalf("guarded sync onto the corrected head: %v\n%s", err, syncOut)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != rewritten {
		t.Fatalf("operator HEAD after sync = %s, want the real remote head %s", got, rewritten)
	}

	t.Logf("=== axi sync --check (rewritten remote) ===\n%s", checkOut)
	t.Logf("=== sync --recover --yes ===\n%s", recoverOut)
	t.Logf("=== axi sync --check (after correction) ===\n%s", afterOut)
	t.Logf("=== axi sync ===\n%s", syncOut)
}

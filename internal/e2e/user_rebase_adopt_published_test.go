//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiUserRebasePublishedOutsidePipelineJourney reproduces issue #1293 with
// the real binary, fake agent, isolated daemon, and local bare push target. A
// run is aborted before the pipeline changes anything, so the branch is
// user_owned while the gate's private mirror still holds the submitted head.
// The operator then publishes the branch directly, rebases it onto a newer
// main with a hand-resolved conflict, and force-pushes the rewrite with a
// lease. No proof can place the superseded head's content in the rewrite, so a
// fresh `axi run` refuses - and before the fix `axi sync --adopt-published`
// refused as well, leaving no supported exit.
//
// The journey: the refusal names the supported command, that command refuses
// while the rewrite is still unpublished, adopts it once the configured push
// target has that exact head, archives the superseded private head the way
// reconciliation does, and a fresh run then validates the rewritten head.
func TestAxiUserRebasePublishedOutsidePipelineJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-user-rebase", "seed.txt", "seed\n", "seed user rebase init")
	initWorktree := h.AddWorktree("init-user-rebase")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/user-rebase"
	branchRef := "refs/heads/" + branch
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	submitted := h.CommitChange(branch, "README.md", "# e2e feature\n", "describe the feature")
	operator := h.AddWorktree(branch)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil || !strings.Contains(out, "state: user_owned") {
		t.Fatalf("abort before any pipeline edit: %v\n%s", err, out)
	}
	if run := h.WaitForRun(branch, 30*time.Second); run.Status != types.RunCancelled {
		t.Fatalf("run status after abort = %s", run.Status)
	}
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if got := git(gateDir, "rev-parse", branchRef); got != submitted {
		t.Fatalf("gate mirror = %s, want the unmoved submitted head %s", got, submitted)
	}

	// The branch is the operator's: publish it directly, then rewrite it
	// outside the pipeline onto a newer main, resolving the conflict by hand.
	git(operator, "push", "origin", "HEAD:"+branchRef)
	h.CommitChange("main", "README.md", "# e2e upstream\n", "advance main")
	git(h.WorkDir, "push", "origin", "main")
	if out, err := h.runGit(context.Background(), operator, "rebase", "main"); err == nil {
		t.Fatalf("rebase onto the advanced main should stop on a conflict:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(operator, "README.md"), []byte("# e2e upstream feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(operator, "add", "README.md")
	git(operator, "-c", "core.editor=true", "rebase", "--continue")
	rebased := git(operator, "rev-parse", "HEAD")
	if _, err := h.runGit(context.Background(), operator, "merge-base", "--is-ancestor", submitted, rebased); err == nil {
		t.Fatalf("rebased head %s still descends from the submitted head %s", rebased, submitted)
	}

	// Unpublished, the rewrite has no exit: the gate will not replace the
	// private head, and adoption has nothing on the push target to verify.
	gateRefs := func() string {
		return git(gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/")
	}
	gateRefsBefore := gateRefs()
	unpublished, err := h.RunInDir(operator, "axi", "sync", "--adopt-published")
	t.Logf("axi sync --adopt-published before publishing the rewrite:\n%s", unpublished)
	if err == nil || !strings.Contains(unpublished, "safety: blocked_published_head_mismatch") {
		t.Fatalf("adoption of an unpublished rewrite should refuse on the target mismatch: %v\n%s", err, unpublished)
	}
	if got := gateRefs(); got != gateRefsBefore {
		t.Fatalf("refused adoption changed gate refs:\nbefore: %s\nafter: %s", gateRefsBefore, got)
	}

	git(operator, "push", "--force-with-lease="+branchRef+":"+submitted, "origin", "HEAD:"+branchRef)
	if got := h.UpstreamBranchSHA(branch); got != rebased {
		t.Fatalf("push target = %s, want the published rewrite %s", got, rebased)
	}

	refused, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the rebased feature")
	t.Logf("axi run on the published rewrite:\n%s", refused)
	if err == nil || !strings.Contains(refused, "refusing to reconcile private mirror ref "+branchRef) {
		t.Fatalf("fresh run should refuse to replace the unproven private head: %v\n%s", err, refused)
	}
	if !strings.Contains(refused, "no-mistakes axi sync --adopt-published") {
		t.Fatalf("refusal does not name the supported exit:\n%s", refused)
	}
	if run := h.ActiveRun(branch); run != nil {
		t.Fatalf("a run started despite the refusal: %+v", run)
	}
	if got := gateRefs(); got != gateRefsBefore {
		t.Fatalf("refused submission changed gate refs:\nbefore: %s\nafter: %s", gateRefsBefore, got)
	}

	adopted, err := h.RunInDir(operator, "axi", "sync", "--adopt-published")
	t.Logf("axi sync --adopt-published on the published rewrite:\n%s", adopted)
	if err != nil {
		t.Fatalf("adopt the published rewrite: %v\n%s", err, adopted)
	}
	for _, want := range []string{"state: user_owned", "changed: true", "code: run_pipeline"} {
		if !strings.Contains(adopted, want) {
			t.Errorf("adoption output missing %q:\n%s", want, adopted)
		}
	}
	if got := git(gateDir, "rev-parse", branchRef); got != rebased {
		t.Fatalf("gate mirror = %s, want the published rewrite %s", got, rebased)
	}
	if got := git(gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/"+branch+"/"+submitted); got != submitted {
		t.Fatalf("superseded private head archived at %s, want %s", got, submitted)
	}
	if got := git(operator, "rev-parse", "HEAD"); got != rebased {
		t.Fatalf("adoption moved the worktree to %s, want %s", got, rebased)
	}
	if got := h.UpstreamBranchSHA(branch); got != rebased {
		t.Fatalf("adoption moved the push target to %s, want %s", got, rebased)
	}

	freshOut, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the rebased feature")
	t.Logf("fresh axi run after adoption:\n%s", freshOut)
	if err != nil || strings.Contains(freshOut, "refusing to reconcile") || !strings.Contains(freshOut, "sync-1") {
		t.Fatalf("fresh submission after adoption did not reach review: %v\n%s", err, freshOut)
	}
	run := h.ActiveRun(branch)
	if run == nil || run.HeadSHA != rebased {
		t.Fatalf("fresh run = %+v, want an active run at head %s", run, rebased)
	}
	h.CancelRun(run.ID)
}

//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAxiRewrittenRemoteRecoveryThenFreshRunJourney drives the published-branch
// rewrite journey with the real binary, fake agent, isolated daemon, and local
// bare push target: a pipeline run publishes a fix commit, someone force-pushes
// a rewrite of that branch outside the pipeline, `axi sync --recover` rebinds
// the push binding and anchors the superseded pipeline head under
// refs/no-mistakes/recover-rewritten/<run>/<generation>, and a fresh `axi run`
// of the rewritten head must then be accepted rather than refused because the
// gate's private mirror still holds the superseded head.
//
// The adversarial half proves the anchor is a preservation credit only: a
// private-only gate commit that no recovery anchor reaches must still refuse
// the fresh submission, naming that commit and not the anchored one.
func TestAxiRewrittenRemoteRecoveryThenFreshRunJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-rewritten", "seed.txt", "seed\n", "seed rewritten init")
	initWorktree := h.AddWorktree("init-rewritten")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/rewritten-journey"
	originalHead := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("review fix: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "approve"); err != nil || !strings.Contains(out, "outcome: passed") {
		t.Fatalf("approve fix review: %v\n%s", err, out)
	}
	pushedHead := h.UpstreamBranchSHA(branch)
	if pushedHead == originalHead {
		t.Fatal("pipeline did not create and push a fix commit")
	}
	gateDir := h.NMHome + "/repos/" + h.repoID() + ".git"
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := git(gateDir, "rev-parse", "refs/heads/"+branch); got != pushedHead {
		t.Fatalf("gate mirror = %s, want published pipeline head %s", got, pushedHead)
	}
	if out, err := h.RunInDir(operator, "axi", "sync"); err != nil || !strings.Contains(out, "state: synchronized") {
		t.Fatalf("sync to pipeline head: %v\n%s", err, out)
	}
	t.Logf("pipeline published %s (operator submitted %s)", pushedHead, originalHead)

	// External rewrite of the published branch, outside the pipeline.
	git(operator, "reset", "--hard", "main")
	if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("rewritten outside the pipeline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(operator, "add", "feature.txt")
	git(operator, "commit", "-m", "rewrite published branch outside the pipeline")
	liveHead := git(operator, "rev-parse", "HEAD")
	git(operator, "push", "--force-with-lease=refs/heads/"+branch+":"+pushedHead, "origin", "HEAD:refs/heads/"+branch)

	checkOut, _ := h.RunInDir(operator, "axi", "sync", "--check")
	t.Logf("axi sync --check after rewrite:\n%s", checkOut)
	if !strings.Contains(checkOut, "remote_rewritten") || !strings.Contains(checkOut, "no-mistakes axi sync --recover") {
		t.Fatalf("rewrite not reported as recoverable:\n%s", checkOut)
	}
	recoverOut, err := h.RunInDir(operator, "axi", "sync", "--recover")
	t.Logf("axi sync --recover:\n%s", recoverOut)
	if err != nil || !strings.Contains(recoverOut, "recovered: true") || !strings.Contains(recoverOut, "proof: worktree") {
		t.Fatalf("rewritten recovery: %v\n%s", err, recoverOut)
	}
	anchors := git(operator, "for-each-ref", "--format=%(refname) %(objectname)", "refs/no-mistakes/recover-rewritten/")
	t.Logf("worktree recovery anchors: %s", anchors)
	if !strings.HasSuffix(anchors, "/1 "+pushedHead) || strings.Contains(anchors, "\n") {
		t.Fatalf("expected one rewritten anchor at %s, got %q", pushedHead, anchors)
	}
	anchorRef := strings.Fields(anchors)[0]
	if got := git(gateDir, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/"); got != "" {
		t.Fatalf("recovery anchored in the gate, not the worktree as the journey requires: %s", got)
	}

	// Adversarial: an unpreserved private-only commit on top of the anchored
	// head must still refuse the fresh submission.
	tree := git(gateDir, "rev-parse", pushedHead+"^{tree}")
	extra := git(gateDir, "-c", "user.name=E2E", "-c", "user.email=e2e@example.com", "commit-tree", tree, "-p", pushedHead, "-m", "unpreserved private-only commit")
	git(gateDir, "update-ref", "refs/heads/"+branch, extra, pushedHead)
	gateRefsBefore := git(gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/")
	refused, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the rewritten branch")
	t.Logf("axi run with an unpreserved private commit:\n%s", refused)
	if err == nil && !strings.Contains(refused, extra) {
		t.Fatalf("fresh run accepted despite an unpreserved private-only commit %s:\n%s", extra, refused)
	}
	if !strings.Contains(refused, extra) {
		t.Fatalf("refusal does not name the unpreserved commit %s:\n%s", extra, refused)
	}
	if strings.Contains(refused, pushedHead) {
		t.Fatalf("refusal still reports the anchored head %s as at risk:\n%s", pushedHead, refused)
	}
	if run := h.ActiveRun(branch); run != nil {
		t.Fatalf("a run started despite the refusal: %+v", run)
	}
	if got := git(gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/"); got != gateRefsBefore {
		t.Fatalf("refused submission changed gate refs:\nbefore: %s\nafter: %s", gateRefsBefore, got)
	}
	git(gateDir, "update-ref", "refs/heads/"+branch, pushedHead, extra)

	// The journey: the fresh submission after a successful recovery starts.
	freshOut, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the rewritten branch")
	t.Logf("fresh axi run after recovery:\n%s", freshOut)
	if err != nil || strings.Contains(freshOut, "refusing to reconcile") || !strings.Contains(freshOut, "sync-1") {
		t.Fatalf("fresh submission after successful recovery did not reach review: %v\n%s", err, freshOut)
	}
	run := h.ActiveRun(branch)
	if run == nil || run.HeadSHA != liveHead {
		t.Fatalf("fresh run = %+v, want an active run at head %s", run, liveHead)
	}
	if got := git(gateDir, "rev-parse", "refs/heads/"+branch); got != liveHead {
		t.Fatalf("gate mirror = %s, want submitted rewritten head %s", got, liveHead)
	}
	archive := git(gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/tags/no-mistakes-abandoned/")
	t.Logf("gate archive of the superseded mirror head: %s", archive)
	if !strings.Contains(archive, pushedHead) {
		t.Fatalf("superseded mirror head %s was not archived before deletion: %q", pushedHead, archive)
	}
	if got := git(operator, "rev-parse", anchorRef); got != pushedHead {
		t.Fatalf("recovery anchor %s moved to %s", anchorRef, got)
	}
	h.CancelRun(run.ID)
}

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

// TestAxiReleasedBranchRewriteResubmitsJourney reproduces issues #1066 and
// #1063 end to end. A run is aborted at the review gate before the pipeline
// changes anything, so its terminal outcome releases the branch as user_owned
// while the gate's branch ref still names the submitted head. The operator
// then prepares the next revision the way a workflow without a push step does:
// rebases onto the moved default branch and amends the commit. The rewritten
// head is neither an ancestor nor a descendant of the submitted head, and its
// patch no longer matches, so an ordinary gate push was refused as
// non-fast-forward and the content proof refused it as at-risk, with status
// reporting user_owned and no plan. The contract: status names the fresh run
// as the next step, recovery stays a no-op that never touches the gate, and the
// fresh `axi run` archives the exact submitted head, submits the rewritten
// branch, and records the archived head as the new run's base.
func TestAxiReleasedBranchRewriteResubmitsJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-released-rewrite", "seed.txt", "seed\n", "seed released rewrite init")
	initWorktree := h.AddWorktree("init-released-rewrite")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	submitted := h.CommitChange("feature/released-rewrite", "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree("feature/released-rewrite")
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature before its next revision")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil {
		t.Fatalf("axi abort: %v\n%s", err, out)
	}
	released := h.WaitForRun("feature/released-rewrite", 30*time.Second)
	if released.Status != types.RunCancelled {
		t.Fatalf("run status after abort = %s", released.Status)
	}
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if got, gitErr := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/feature/released-rewrite"); gitErr != nil || strings.TrimSpace(string(got)) != submitted {
		t.Fatalf("gate branch = %s (err %v), want the submitted head %s", strings.TrimSpace(string(got)), gitErr, submitted)
	}

	// The default branch moves on; the next revision is rebased onto it and
	// amended, which is how a workflow that publishes through a review server
	// prepares every patch set.
	h.CommitChange("main", "upstream-advance.txt", "advance\n", "upstream advance")
	if out, gitErr := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); gitErr != nil {
		t.Fatalf("advance upstream main: %v\n%s", gitErr, out)
	}
	if out, gitErr := h.runGit(context.Background(), operator, "rebase", "main"); gitErr != nil {
		t.Fatalf("rebase onto the advanced default branch: %v\n%s", gitErr, out)
	}
	if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("unsafe, revised\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, gitErr := h.runGit(context.Background(), operator, "commit", "-a", "--amend", "--no-edit"); gitErr != nil {
		t.Fatalf("amend the next revision: %v\n%s", gitErr, out)
	}
	rewrittenBytes, gitErr := h.runGit(context.Background(), operator, "rev-parse", "HEAD")
	if gitErr != nil {
		t.Fatalf("rev-parse rewritten head: %v", gitErr)
	}
	rewritten := strings.TrimSpace(string(rewrittenBytes))
	if _, ancErr := h.runGit(context.Background(), operator, "merge-base", "--is-ancestor", submitted, rewritten); ancErr == nil {
		t.Fatalf("fixture did not rewrite history: %s descends from %s", rewritten, submitted)
	}

	// Status names the fresh run as the next step instead of reporting a
	// released branch with no plan.
	statusOut, err := h.RunInDir(operator, "axi", "status")
	if err != nil {
		t.Fatalf("axi status: %v\n%s", err, statusOut)
	}
	for _, want := range []string{"state: user_owned", "relation: diverged", "safety: stale_submitted_mirror", "code: run_pipeline"} {
		if !strings.Contains(statusOut, want) {
			t.Errorf("status output missing %q:\n%s", want, statusOut)
		}
	}
	checkOut, err := h.RunInDir(operator, "axi", "sync", "--check")
	if err != nil {
		t.Fatalf("released sync --check must exit zero: %v\n%s", err, checkOut)
	}
	for _, want := range []string{"state: user_owned", "safety: stale_submitted_mirror", "code: run_pipeline"} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("check output missing %q:\n%s", want, checkOut)
		}
	}

	// Recovery is still an idempotent no-op: it never moves the gate lane.
	recoverOut, err := h.RunInDir(operator, "axi", "sync", "--recover", "--keep-local")
	if err != nil {
		t.Fatalf("released recover: %v\n%s", err, recoverOut)
	}
	for _, want := range []string{"recovered: true", "changed: false", "state: user_owned", "code: run_pipeline"} {
		if !strings.Contains(recoverOut, want) {
			t.Errorf("recover output missing %q:\n%s", want, recoverOut)
		}
	}
	if got, gitErr := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/feature/released-rewrite"); gitErr != nil || strings.TrimSpace(string(got)) != submitted {
		t.Fatalf("recovery moved the gate branch to %s (err %v), want untouched %s", strings.TrimSpace(string(got)), gitErr, submitted)
	}

	// The fresh run archives the exact submitted head, submits the rewritten
	// branch, and parks at the review gate like any other submission.
	freshOut, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the rewritten next revision")
	if err != nil || !strings.Contains(freshOut, "sync-1") {
		t.Fatalf("fresh submission of the rewritten released branch: %v\n%s", err, freshOut)
	}
	if got, gitErr := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/feature/released-rewrite"); gitErr != nil || strings.TrimSpace(string(got)) != rewritten {
		t.Fatalf("gate branch = %s (err %v), want the rewritten head %s", strings.TrimSpace(string(got)), gitErr, rewritten)
	}
	archiveTag := "refs/tags/no-mistakes-abandoned/feature/released-rewrite/" + submitted
	if got, gitErr := h.runGit(context.Background(), gateDir, "rev-parse", archiveTag); gitErr != nil || strings.TrimSpace(string(got)) != submitted {
		t.Fatalf("archive %s = %s (err %v), want the submitted head %s archived before replacement", archiveTag, strings.TrimSpace(string(got)), gitErr, submitted)
	}
	var fresh *types.RunStatus
	for _, run := range h.Runs() {
		if run.HeadSHA != rewritten {
			continue
		}
		status := run.Status
		fresh = &status
		if run.BaseSHA != submitted {
			t.Fatalf("fresh run base = %s, want the archived submitted head %s", run.BaseSHA, submitted)
		}
	}
	if fresh == nil {
		t.Fatalf("no run recorded for the rewritten head %s", rewritten)
	}
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil {
		t.Fatalf("cleanup abort: %v\n%s", err, out)
	}
}

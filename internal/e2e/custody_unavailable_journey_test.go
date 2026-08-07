//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiUnavailablePreservedHeadCustodyReleaseJourney reproduces the exact
// operator-visible closed loop left by a failed terminal run whose recorded
// pipeline head no longer exists in any no-mistakes-owned Git object store.
// The local branch and configured remote are equal and clean, while an
// independent safety repository retains the lost pipeline commit as evidence
// that the fixture itself did not discard it.
func TestAxiUnavailablePreservedHeadCustodyReleaseJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.CommitChange("init-unavailable-custody", "seed.txt", "seed\n", "seed unavailable custody init")
	initWorktree := h.AddWorktree("init-unavailable-custody")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/unavailable-custody"
	submitted := h.CommitChange(branch, "feature.txt", "operator work remains safe\n", "add operator work")
	operator := h.AddWorktree(branch)
	if out, err := h.runGit(context.Background(), operator, "push", "origin", branch); err != nil {
		t.Fatalf("push operator branch to configured remote: %v\n%s", err, out)
	}
	if remote := h.UpstreamBranchSHA(branch); remote != submitted {
		t.Fatalf("remote head = %s, want local submitted head %s", remote, submitted)
	}

	p := paths.WithRoot(h.NMHome)
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if out, err := h.runGit(context.Background(), gateDir, "fetch", "--no-tags", "--no-write-fetch-head", operator, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		t.Fatalf("seed gate branch without firing receive hooks: %v\n%s", err, out)
	}
	pipeline := filepath.Join(t.TempDir(), "pipeline")
	if out, err := h.runGit(context.Background(), filepath.Dir(pipeline), "clone", gateDir, pipeline); err != nil {
		t.Fatalf("clone isolated pipeline worktree: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), pipeline, "checkout", branch); err != nil {
		t.Fatalf("checkout pipeline branch: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(pipeline, "pipeline-fix.txt"), []byte("pipeline-only fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(context.Background(), pipeline, "add", "pipeline-fix.txt"); err != nil {
		t.Fatalf("stage pipeline fix: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), pipeline, "commit", "-m", "no-mistakes(review): preserve a fix"); err != nil {
		t.Fatalf("commit pipeline fix: %v\n%s", err, out)
	}
	preservedBytes, err := h.runGit(context.Background(), pipeline, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve pipeline head: %v\n%s", err, preservedBytes)
	}
	preserved := strings.TrimSpace(string(preservedBytes))
	if out, err := h.runGit(context.Background(), gateDir, "fetch", "--no-tags", "--no-write-fetch-head", pipeline, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		t.Fatalf("record pipeline head in gate: %v\n%s", err, out)
	}

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.GetRepo(h.repoID())
	if err != nil || repo == nil {
		database.Close()
		t.Fatalf("load registered repo: %#v, %v", repo, err)
	}
	baseBytes, err := h.runGit(context.Background(), operator, "rev-parse", "main")
	if err != nil {
		database.Close()
		t.Fatalf("resolve base: %v\n%s", err, baseBytes)
	}
	run, err := database.InsertRun(repo.ID, branch, submitted, strings.TrimSpace(string(baseBytes)))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, preserved); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Keep a separately rooted safety branch, then remove every copy owned by
	// no-mistakes: no local recovery ref, no gate object, and no managed
	// worktree. This is the initiating trigger; the stale run row is the masking
	// condition that continues to classify the branch as pipeline-owned.
	safety := filepath.Join(t.TempDir(), "independent-safety.git")
	if out, err := h.runGit(context.Background(), filepath.Dir(safety), "init", "--bare", safety); err != nil {
		t.Fatalf("init independent safety repo: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), pipeline, "push", safety, "HEAD:refs/heads/safety/"+run.ID); err != nil {
		t.Fatalf("preserve independent safety branch: %v\n%s", err, out)
	}
	if err := os.RemoveAll(pipeline); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(context.Background(), gateDir, "update-ref", "refs/heads/"+branch, submitted, preserved); err != nil {
		t.Fatalf("move stale gate branch away from unavailable head: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"reflog", "expire", "--expire=now", "--all"}, {"gc", "--prune=now"}} {
		if out, err := h.runGit(context.Background(), gateDir, args...); err != nil {
			t.Fatalf("git %s in gate: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if _, err := h.runGit(context.Background(), operator, "cat-file", "-e", preserved+"^{commit}"); err == nil {
		t.Fatal("fixture left the recorded preserved head in the operator repository")
	}
	if _, err := h.runGit(context.Background(), gateDir, "cat-file", "-e", preserved+"^{commit}"); err == nil {
		t.Fatal("fixture left the recorded preserved head in the gate object store")
	}
	if got, err := h.runGit(context.Background(), safety, "rev-parse", "refs/heads/safety/"+run.ID); err != nil || strings.TrimSpace(string(got)) != preserved {
		t.Fatalf("independent safety head = %s (err %v), want %s", strings.TrimSpace(string(got)), err, preserved)
	}
	if out, err := h.runGit(context.Background(), operator, "status", "--porcelain"); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("operator worktree not clean: %q (err %v)", string(out), err)
	}

	statusOut, err := h.RunInDir(operator, "axi", "status")
	if err != nil {
		t.Fatalf("axi status: %v\n%s", err, statusOut)
	}
	for _, want := range []string{"state: pipeline_owned", "status: failed", "safety: blocked_pipeline_owned_recoverable", "code: recover_custody"} {
		if !strings.Contains(statusOut, want) {
			t.Errorf("status missing %q:\n%s", want, statusOut)
		}
	}

	abortOut, err := h.RunInDir(operator, "axi", "abort", "--run", run.ID)
	if err != nil {
		t.Fatalf("terminal abort lookup: %v\n%s", err, abortOut)
	}
	for _, want := range []string{"aborted: false", "run_status: failed", "already terminal"} {
		if !strings.Contains(abortOut, want) {
			t.Errorf("terminal abort lookup missing %q:\n%s", want, abortOut)
		}
	}

	recoverOut, recoverErr := h.RunInDir(operator, "axi", "sync", "--recover")
	if recoverErr == nil {
		t.Fatalf("ordinary recovery unexpectedly succeeded without the preserved head:\n%s", recoverOut)
	}
	for _, want := range []string{
		"safety: blocked_recover_gate_diverged",
		preserved,
		"code: release_unavailable_custody",
		"no-mistakes axi sync --release-unavailable --run " + run.ID,
	} {
		if !strings.Contains(recoverOut, want) {
			t.Fatalf("ordinary recovery did not expose unavailable-head next action %q:\n%s", want, recoverOut)
		}
	}

	runsBefore := len(h.Runs())
	blockedRunOut, blockedRunErr := h.RunInDir(operator, "axi", "run", "--intent", "complete the independently preserved operator correction")
	if blockedRunErr == nil {
		t.Fatalf("fresh run unexpectedly escaped terminal custody:\n%s", blockedRunOut)
	}
	for _, want := range []string{"state: pipeline_owned", "status: failed", "safety: blocked_pipeline_owned_recoverable", "command: no-mistakes axi sync --recover"} {
		if !strings.Contains(blockedRunOut, want) {
			t.Errorf("blocked fresh run missing %q:\n%s", want, blockedRunOut)
		}
	}
	if got := len(h.Runs()); got != runsBefore {
		t.Fatalf("blocked fresh run changed run count from %d to %d", runsBefore, got)
	}
	t.Logf("reproduced closed loop: status points to recovery; abort=%q; recover refuses unavailable %s; fresh run points back to recovery", strings.TrimSpace(abortOut), preserved)

	releaseOut, err := h.RunInDir(operator, "axi", "sync", "--release-unavailable", "--run", run.ID)
	if err != nil {
		t.Fatalf("identity-bound unavailable-head release: %v\n%s", err, releaseOut)
	}
	for _, want := range []string{
		"state: custody_returned",
		"released: true",
		"action: release_unavailable",
		"reason: preserved_head_unavailable",
		"run: \"" + run.ID + "\"",
		"idempotent: false",
		"refs/no-mistakes/custody-release/" + run.ID + "/local",
		"refs/no-mistakes/custody-release/" + run.ID + "/gate",
	} {
		if !strings.Contains(releaseOut, want) {
			t.Errorf("release output missing %q:\n%s", want, releaseOut)
		}
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Fatalf("release moved operator branch to %s, want %s", got, submitted)
	}
	if remote := h.UpstreamBranchSHA(branch); remote != submitted {
		t.Fatalf("release changed configured remote to %s, want %s", remote, submitted)
	}
	if got, err := h.runGit(context.Background(), safety, "rev-parse", "refs/heads/safety/"+run.ID); err != nil || strings.TrimSpace(string(got)) != preserved {
		t.Fatalf("release changed independent safety head to %s (err %v)", strings.TrimSpace(string(got)), err)
	}

	retryOut, err := h.RunInDir(operator, "axi", "sync", "--release-unavailable", "--run", run.ID)
	if err != nil {
		t.Fatalf("idempotent release retry: %v\n%s", err, retryOut)
	}
	for _, want := range []string{"released: true", "action: release_unavailable", "idempotent: true", "changed: false"} {
		if !strings.Contains(retryOut, want) {
			t.Errorf("release retry missing %q:\n%s", want, retryOut)
		}
	}

	freshOut, err := h.RunInDir(operator, "axi", "run", "--intent", "complete the independently preserved operator correction")
	if err != nil {
		t.Fatalf("fresh run after explicit release: %v\n%s", err, freshOut)
	}
	if strings.Contains(freshOut, "blocked_pipeline_owned_recoverable") || !strings.Contains(freshOut, "outcome: passed") {
		t.Fatalf("fresh run remained in the closed loop:\n%s", freshOut)
	}
}

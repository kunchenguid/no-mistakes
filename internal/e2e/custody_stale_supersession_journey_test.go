//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiStaleCustodySupersessionJourney proves the operator-visible contract:
// read-only --check returns one exact old/later command, that command preserves
// every head and adopts only exact persisted run edges, and a retry is
// idempotent. The fixture uses the exact two-run chain needed to recover this
// PR's own dogfood branch; the three-run rebased incident is covered with
// independent Git and database oracles in internal/branchsync.
func TestAxiStaleCustodySupersessionJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.CommitChange("init-stale-custody", "seed.txt", "seed\n", "seed stale custody init")
	initWorktree := h.AddWorktree("init-stale-custody")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/stale-custody"
	oldSubmitted := h.CommitChange(branch, "operator.txt", "operator work\n", "operator submission")
	operator := h.AddWorktree(branch)
	p := paths.WithRoot(h.NMHome)
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if out, err := h.runGit(context.Background(), gateDir, "fetch", "--no-tags", "--no-write-fetch-head", operator, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		t.Fatalf("seed gate: %v\n%s", err, out)
	}

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.GetRepo(h.repoID())
	if err != nil || repo == nil {
		database.Close()
		t.Fatalf("registered repo = %#v, %v", repo, err)
	}
	baseBytes, err := h.runGit(context.Background(), operator, "rev-parse", "main")
	if err != nil {
		database.Close()
		t.Fatalf("resolve base: %v\n%s", err, baseBytes)
	}
	oldRun, err := database.InsertRun(repo.ID, branch, oldSubmitted, strings.TrimSpace(string(baseBytes)))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}

	oldPipeline := filepath.Join(t.TempDir(), "old-pipeline")
	if out, err := h.runGit(context.Background(), filepath.Dir(oldPipeline), "clone", gateDir, oldPipeline); err != nil {
		database.Close()
		t.Fatalf("clone old pipeline: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), oldPipeline, "checkout", branch); err != nil {
		database.Close()
		t.Fatalf("checkout old pipeline: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(oldPipeline, "old-fix.txt"), []byte("old pipeline fix\n"), 0o644); err != nil {
		database.Close()
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "old-fix.txt"}, {"commit", "-m", "no-mistakes(review): old fix"}} {
		if out, err := h.runGit(context.Background(), oldPipeline, args...); err != nil {
			database.Close()
			t.Fatalf("old pipeline git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	oldHeadBytes, err := h.runGit(context.Background(), oldPipeline, "rev-parse", "HEAD")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	oldHead := strings.TrimSpace(string(oldHeadBytes))
	if out, err := h.runGit(context.Background(), gateDir, "fetch", "--no-tags", "--no-write-fetch-head", oldPipeline, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		database.Close()
		t.Fatalf("advance gate to old pipeline head without receive hook: %v\n%s", err, out)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(oldRun.ID, types.RunFailed, oldHead); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if out, err := h.runGit(context.Background(), operator, "fetch", gateDir, oldHead+":refs/no-mistakes/recover/"+oldRun.ID); err != nil {
		database.Close()
		t.Fatalf("preserve old head locally: %v\n%s", err, out)
	}

	laterRun, err := database.InsertRun(repo.ID, branch, oldHead, strings.TrimSpace(string(baseBytes)))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	laterPipeline := filepath.Join(t.TempDir(), "later-pipeline")
	if out, err := h.runGit(context.Background(), filepath.Dir(laterPipeline), "clone", gateDir, laterPipeline); err != nil {
		database.Close()
		t.Fatalf("clone later pipeline: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), laterPipeline, "checkout", branch); err != nil {
		database.Close()
		t.Fatalf("checkout later pipeline: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(laterPipeline, "later-fix.txt"), []byte("later exact fix\n"), 0o644); err != nil {
		database.Close()
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "later-fix.txt"}, {"commit", "-m", "no-mistakes(review): later fix"}, {"push", h.UpstreamDir, "HEAD:refs/heads/" + branch}} {
		if out, err := h.runGit(context.Background(), laterPipeline, args...); err != nil {
			database.Close()
			t.Fatalf("later pipeline git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if out, err := h.runGit(context.Background(), gateDir, "fetch", "--no-tags", "--no-write-fetch-head", laterPipeline, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		database.Close()
		t.Fatalf("advance gate to later pushed head without receive hook: %v\n%s", err, out)
	}
	laterHeadBytes, err := h.runGit(context.Background(), laterPipeline, "rev-parse", "HEAD")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	laterHead := strings.TrimSpace(string(laterHeadBytes))
	if err := database.UpdateRunHeadSHA(laterRun.ID, laterHead); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(laterRun.ID, db.PushBinding{
		HeadSHA: laterHead, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(repo.PushURL()), Ref: "refs/heads/" + branch,
	}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(laterRun.ID, types.RunFailed, laterHead); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	localRefsBefore, _ := h.runGit(context.Background(), operator, "for-each-ref", "--format=%(refname) %(objectname)")
	gateRefsBefore, _ := h.runGit(context.Background(), gateDir, "for-each-ref", "--format=%(refname) %(objectname)")
	checkOut, err := h.RunInDir(operator, "axi", "sync", "--check")
	if err != nil {
		t.Fatalf("read-only stale-custody plan: %v\n%s", err, checkOut)
	}
	for _, want := range []string{
		"safety: safe_stale_custody_supersession",
		"code: supersede_stale_custody",
		"no-mistakes axi sync --supersede-stale --run " + oldRun.ID + " --later-run " + laterRun.ID,
		"action: supersede_stale",
		"preserved_head: " + oldHead,
		"submitted_head: " + oldHead,
		"pushed_head: " + laterHead,
	} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("read-only plan missing %q:\n%s", want, checkOut)
		}
	}
	if refs, _ := h.runGit(context.Background(), operator, "for-each-ref", "--format=%(refname) %(objectname)"); string(refs) != string(localRefsBefore) {
		t.Fatal("read-only plan changed operator refs")
	}
	if refs, _ := h.runGit(context.Background(), gateDir, "for-each-ref", "--format=%(refname) %(objectname)"); string(refs) != string(gateRefsBefore) {
		t.Fatal("read-only plan changed gate refs")
	}

	transitionOut, err := h.RunInDir(operator, "axi", "sync", "--supersede-stale", "--run", oldRun.ID, "--later-run", laterRun.ID)
	if err != nil {
		t.Fatalf("stale-custody transition: %v\n%s", err, transitionOut)
	}
	for _, want := range []string{
		"state: synchronized", "changed: true", "released: true", "reason: stale_owner_superseded", "idempotent: false",
		"refs/no-mistakes/custody-supersede/" + oldRun.ID + "/preserved-local/" + oldHead,
		"refs/no-mistakes/custody-supersede/" + oldRun.ID + "/preserved-gate/" + oldHead,
		"refs/no-mistakes/custody-supersede/" + oldRun.ID + "/local/" + oldSubmitted,
	} {
		if !strings.Contains(transitionOut, want) {
			t.Errorf("transition missing %q:\n%s", want, transitionOut)
		}
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != laterHead {
		t.Fatalf("operator branch = %s, want exact later push %s", got, laterHead)
	}
	if got := h.UpstreamBranchSHA(branch); got != laterHead {
		t.Fatalf("transition changed remote head to %s, want %s", got, laterHead)
	}

	retryOut, err := h.RunInDir(operator, "axi", "sync", "--supersede-stale", "--run", oldRun.ID, "--later-run", laterRun.ID)
	if err != nil {
		t.Fatalf("idempotent retry: %v\n%s", err, retryOut)
	}
	for _, want := range []string{"released: true", "changed: false", "idempotent: true", "reason: stale_owner_superseded"} {
		if !strings.Contains(retryOut, want) {
			t.Errorf("retry missing %q:\n%s", want, retryOut)
		}
	}
}

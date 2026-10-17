//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Both targets are disposable bare repositories. Rejecting the original
// target models a publication failure without using credentials or the network.
func TestForkChangeDoesNotRebindRunningExecutor(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	ctx := context.Background()
	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	forkDir := filepath.Join(t.TempDir(), "fork.git")
	mustJourneyGit(t, h, h.WorkDir, "init", "--bare", "--initial-branch=main", forkDir)
	mustJourneyGit(t, h, h.WorkDir, "push", forkDir, "main")
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	mustJourneyGit(t, h, h.WorkDir, "remote", "set-url", "origin", parentURL)
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_PARENT", "parent-owner/no-mistakes")
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(t.TempDir(), "gh.log"))
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/changed-fork"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "submit feature")
	out, err := h.Run("axi", "run", "--intent", "guard the feature")
	if err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("review gate: %v\n%s", err, out)
	}
	first := h.ActiveRun(branch)
	if first == nil {
		t.Fatal("no active run")
	}
	// This changes registration, not the executor's already-held context.
	initOut, err := h.Run("init", "--fork-url", forkURL)
	if err != nil {
		t.Fatalf("refresh fork: %v\n%s", err, initOut)
	}
	t.Logf("fork refresh while run %s is active:\n%s", first.ID, initOut)
	if err := os.WriteFile(filepath.Join(h.UpstreamDir, "hooks", "pre-receive"), []byte("#!/bin/sh\necho original-target-refused >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("fix gate: %v\n%s", err, out)
	}
	out, err = h.Run("axi", "respond", "--action", "approve")
	if err == nil || !strings.Contains(out, "original-target-refused") || !strings.Contains(out, parentURL) {
		t.Fatalf("expected original target refusal: %v\n%s", err, out)
	}
	t.Logf("immutable executor target refusal:\n%s", out)
	run := journeyStoredRun(t, h, h.WaitForRun(branch, 30*time.Second).ID)
	if run.ID != first.ID || run.Status != types.RunFailed || run.LastPushedSHA != nil {
		t.Fatalf("unexpected failed run: %+v", run)
	}
	for _, target := range []string{h.UpstreamDir, forkDir} {
		if out, err := h.runGit(ctx, target, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
			t.Fatalf("failed run published to %s: %s", target, out)
		}
	}
	if out, err := h.Run("axi", "sync", "--recover"); err != nil {
		t.Fatalf("recover original failed run: %v\n%s", err, out)
	}
	plan := filepath.Join(t.TempDir(), "verification.txt")
	if err := os.WriteFile(plan, []byte("Verify the guarded feature and publication to the configured fork, never the parent.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = h.Run("axi", "run", "--intent", "validate recovered work and publish to the configured fork", "--verification-plan", plan)
	if err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("fresh fork run: %v\n%s", err, out)
	}
	if out, err := h.Run("axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("approve fresh review: %v\n%s", err, out)
	}
	fresh := journeyStoredRun(t, h, h.WaitForRun(branch, 30*time.Second).ID)
	if fresh.ID == run.ID || fresh.LastPushedSHA == nil || fresh.VerificationPlan == nil || fresh.PushTargetKind == nil || *fresh.PushTargetKind != "fork" {
		t.Fatalf("fresh run lost publication or verification binding: %+v", fresh)
	}
	if got := mustJourneyGit(t, h, forkDir, "rev-parse", "refs/heads/"+branch); got != *fresh.LastPushedSHA {
		t.Fatalf("fork head %s != exact pushed head %s", got, *fresh.LastPushedSHA)
	}
	if out, err := h.runGit(ctx, h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("fresh run published to parent: %s", out)
	}
	if !strings.Contains(initOut, "running executors keep their original publication target") {
		t.Fatalf("fork refresh did not explain immutable running target:\n%s", initOut)
	}
}

// Seed only terminal history in task-private state. Each head contains a
// different correction; neither is a replacement for the other.
func TestAxiDivergentTerminalFixesKeepBothHistories(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/two-fixes"
	submitted := h.CommitChange(branch, "feature.txt", "feature\n", "submit feature")
	earlier := h.CommitChange(branch, "hardlink-guard.txt", "reject hardlinked records\n", "earlier validated safety fix")
	writer := filepath.Join(t.TempDir(), "writer")
	mustJourneyGit(t, h, h.WorkDir, "clone", h.WorkDir, writer)
	mustJourneyGit(t, h, writer, "config", "user.name", "E2E")
	mustJourneyGit(t, h, writer, "config", "user.email", "e2e@example.com")
	mustJourneyGit(t, h, writer, "checkout", "-b", "later", submitted)
	if err := os.WriteFile(filepath.Join(writer, "snapshot-guard.txt"), []byte("retain latest review evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustJourneyGit(t, h, writer, "add", ".")
	mustJourneyGit(t, h, writer, "commit", "-m", "later validated snapshot fix")
	later := mustJourneyGit(t, h, writer, "rev-parse", "HEAD")
	gateDir := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
	mustJourneyGit(t, h, gateDir, "fetch", h.WorkDir, submitted+":refs/heads/"+branch)
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	base := mustJourneyGit(t, h, h.WorkDir, "rev-parse", submitted+"^")
	var newer *db.Run
	for i, head := range []string{earlier, later} {
		run, err := database.InsertRun(h.repoID(), branch, submitted, base)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, head); err != nil {
			t.Fatal(err)
		}
		source := h.WorkDir
		if i == 1 {
			source = writer
		}
		mustJourneyGit(t, h, gateDir, "fetch", source, head+":"+custody.RecoveryRef(run.ID))
		if i == 0 {
			if err := database.SetRunsCustodyReturned([]string{run.ID}); err != nil {
				t.Fatal(err)
			}
		} else {
			newer = run
		}
	}
	refsBefore := mustJourneyGit(t, h, gateDir, "show-ref")
	countBefore := len(h.Runs())
	out, err := h.Run("rerun")
	if err == nil || !strings.Contains(out, "differs from clean local head") || !strings.Contains(out, later) || !strings.Contains(out, earlier) {
		t.Fatalf("clean caller mismatch was not refused: %v\n%s", err, out)
	}
	t.Logf("supported rerun refusal (already fixed by 68ae62f18d6d2925fffaa680fec5f2c7ab0cc56d):\n%s", out)
	if len(h.Runs()) != countBefore || mustJourneyGit(t, h, gateDir, "show-ref") != refsBefore {
		t.Fatal("refused rerun changed history or started a run")
	}
	out, err = h.Run("axi", "run", "--intent", "validate the intended local fixes without replacing preserved pipeline work")
	if err == nil || !strings.Contains(out, "state: pipeline_owned") || len(h.Runs()) != countBefore || mustJourneyGit(t, h, gateDir, "show-ref") != refsBefore {
		t.Fatalf("terminal attachment started or replaced work: %v\n%s", err, out)
	}
	t.Logf("terminal attachment refusal:\n%s", out)
	out, err = h.Run("axi", "sync", "--check")
	if err == nil {
		t.Fatalf("unreturned custody check passed:\n%s", out)
	}
	t.Logf("divergent terminal custody diagnostic:\n%s", out)
	if !strings.Contains(out, "command: no-mistakes axi sync --recover --keep-local") {
		t.Errorf("safe available-head keep-local continuation was not offered:\n%s", out)
	}
	// A symbolic private lane must not redirect custody return to another branch.
	mustJourneyGit(t, h, gateDir, "update-ref", "refs/heads/bystander", submitted)
	mustJourneyGit(t, h, gateDir, "symbolic-ref", "refs/heads/"+branch, "refs/heads/bystander")
	symbolicLocalRefs := mustJourneyGit(t, h, h.WorkDir, "show-ref")
	symbolicGateRefs := mustJourneyGit(t, h, gateDir, "show-ref")
	out, err = h.Run("axi", "sync", "--recover", "--keep-local")
	if err == nil || !strings.Contains(out, "blocked_recover_gate_race") ||
		mustJourneyGit(t, h, h.WorkDir, "show-ref") != symbolicLocalRefs || mustJourneyGit(t, h, gateDir, "show-ref") != symbolicGateRefs ||
		journeyStoredRun(t, h, newer.ID).CustodyReturnedAt != nil {
		t.Fatalf("symbolic lane recovery mutated state: %v\n%s", err, out)
	}
	t.Logf("symbolic lane refusal without Git mutation:\n%s", out)
	mustJourneyGit(t, h, gateDir, "update-ref", "--no-deref", "refs/heads/"+branch, submitted, submitted)
	out, err = h.Run("axi", "sync", "--recover")
	if err == nil || !strings.Contains(out, "blocked_recover_diverged") {
		t.Fatalf("plain recovery took a head missing local work: %v\n%s", err, out)
	}
	out, err = h.Run("axi", "sync", "--recover", "--keep-local")
	if err != nil || !strings.Contains(out, "recovered: true") || !strings.Contains(out, "changed: false") {
		t.Fatalf("explicit non-discarding keep-local: %v\n%s", err, out)
	}
	t.Logf("non-discarding keep-local result:\n%s", out)
	if mustJourneyGit(t, h, h.WorkDir, "rev-parse", "HEAD") != earlier || mustJourneyGit(t, h, h.WorkDir, "rev-parse", custody.RecoveryRef(newer.ID)) != later {
		t.Fatal("keep-local dropped a correction history")
	}
	if len(h.Runs()) != countBefore {
		t.Fatal("custody recovery started a run")
	}
	localRefs := mustJourneyGit(t, h, h.WorkDir, "show-ref")
	gateRefs := mustJourneyGit(t, h, gateDir, "show-ref")
	if out, err := h.Run("axi", "sync", "--recover", "--keep-local"); err != nil || !strings.Contains(out, "changed: false") {
		t.Fatalf("repeat recovery not a no-op: %v\n%s", err, out)
	}
	if mustJourneyGit(t, h, h.WorkDir, "show-ref") != localRefs || mustJourneyGit(t, h, gateDir, "show-ref") != gateRefs {
		t.Fatal("repeat recovery changed refs")
	}
	if out, err := h.runGit(context.Background(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("custody operations published without validation: %s", out)
	}
	mustJourneyGit(t, h, h.WorkDir, "merge", "--no-ff", "-m", "retain both correction histories", custody.RecoveryRef(newer.ID))
	for _, head := range []string{earlier, later} {
		mustJourneyGit(t, h, h.WorkDir, "merge-base", "--is-ancestor", head, "HEAD")
	}
	for _, file := range []string{"hardlink-guard.txt", "snapshot-guard.txt"} {
		if _, err := os.Stat(filepath.Join(h.WorkDir, file)); err != nil {
			t.Fatal(err)
		}
	}
	plan := filepath.Join(t.TempDir(), "combined-verification.txt")
	if err := os.WriteFile(plan, []byte("Verify both correction histories and their retained guard files on the combined head.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = h.Run("axi", "run", "--intent", "validate both retained safety fixes together before publication", "--verification-plan", plan)
	if err != nil || !strings.Contains(out, "outcome: passed") {
		t.Fatalf("combined fresh full validation: %v\n%s", err, out)
	}
	fresh := journeyStoredRun(t, h, h.WaitForRun(branch, 30*time.Second).ID)
	if fresh.ID == newer.ID || fresh.VerificationPlan == nil || fresh.LastPushedSHA == nil || *fresh.LastPushedSHA != fresh.HeadSHA {
		t.Fatalf("combined run lacks fresh verification or exact push identity: %+v", fresh)
	}
	for _, head := range []string{earlier, later} {
		mustJourneyGit(t, h, h.UpstreamDir, "merge-base", "--is-ancestor", head, fresh.HeadSHA)
	}
	t.Logf("combined fresh full-validation result:\n%s", out)
}

func journeyStoredRun(t *testing.T, h *Harness, id string) *db.Run {
	t.Helper()
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(id)
	if err != nil || run == nil {
		t.Fatalf("stored run %s: %+v, %v", id, run, err)
	}
	return run
}

func mustJourneyGit(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

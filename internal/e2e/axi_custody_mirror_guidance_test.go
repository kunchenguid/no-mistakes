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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func mirrorGuidanceSnapshot(t *testing.T, h *Harness, gateDir, operator string) string {
	t.Helper()
	var parts []string
	for _, dir := range []string{gateDir, operator} {
		refs, err := h.runGit(context.Background(), dir, "for-each-ref", "--format=%(refname) %(objectname)")
		if err != nil {
			t.Fatalf("for-each-ref in %s: %v\n%s", dir, err, refs)
		}
		objects, err := h.runGit(context.Background(), dir, "count-objects", "-v")
		if err != nil {
			t.Fatalf("count-objects in %s: %v\n%s", dir, err, objects)
		}
		parts = append(parts, string(refs), string(objects))
	}
	return strings.Join(parts, "\n--\n")
}

func TestAxiCustodyReturnedGuidanceMatchesFreshSubmissionAdmission(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: rebaseCustodyScenario(t)})
	ctx := context.Background()
	h.CommitChange("init-mirror-guidance", "seed.txt", "seed\n", "seed mirror guidance init")
	initWorktree := h.AddWorktree("init-mirror-guidance")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	h.CommitChange("main", "mode.txt", "canary\n", "seed mode file")
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("publish mode file: %v\n%s", err, out)
	}
	h.CommitChange("feature/mirror-guidance", "feature.txt", "unsafe\n", "add unsafe feature")
	submitted := h.CommitChange("feature/mirror-guidance", "mode.txt", "outreach\n", "switch mode to outreach")
	h.CommitChange("main", "upstream-advance.txt", "advance\n", "upstream advance")
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("advance upstream main: %v\n%s", err, out)
	}

	operator := h.AddWorktree("feature/mirror-guidance")
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature before cancellation")
	if err != nil || !strings.Contains(gateOut, "rebase-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "rebase-1"); err != nil {
		t.Fatalf("review fix: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil {
		t.Fatalf("axi abort: %v\n%s", err, out)
	}
	run := h.WaitForRun("feature/mirror-guidance", 30*time.Second)
	if run.Status != types.RunCancelled {
		t.Fatalf("run status after abort = %s", run.Status)
	}

	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	preserved := strings.TrimSpace(string(mustOutput(t)(h.runGit(ctx, gateDir, "rev-parse", custody.RecoveryRef(run.ID)))))
	if _, ancErr := h.runGit(ctx, gateDir, "merge-base", "--is-ancestor", submitted, preserved); ancErr == nil {
		t.Fatalf("pipeline did not rebase: preserved %s still descends from submitted %s", preserved, submitted)
	}

	recoverOut, err := h.RunInDir(operator, "axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, recoverOut)
	}
	if !strings.Contains(recoverOut, "recovered: true") || !strings.Contains(recoverOut, "state: custody_returned") {
		t.Fatalf("custody was not returned:\n%s", recoverOut)
	}

	if err := os.WriteFile(filepath.Join(operator, "mode.txt"), []byte("canary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, gitErr := h.runGit(ctx, operator, "commit", "-am", "deliberately return to canary"); gitErr != nil {
		t.Fatalf("operator revert: %v\n%s", gitErr, out)
	}
	local := strings.TrimSpace(string(mustOutput(t)(h.runGit(ctx, operator, "rev-parse", "HEAD"))))
	if laneHead := strings.TrimSpace(string(mustOutput(t)(h.runGit(ctx, gateDir, "rev-parse", "refs/heads/feature/mirror-guidance")))); laneHead != submitted {
		t.Fatalf("gate lane = %s, want the submitted head %s", laneHead, submitted)
	}
	if _, ancErr := h.runGit(ctx, operator, "merge-base", "--is-ancestor", preserved, local); ancErr != nil {
		t.Fatalf("local head %s does not descend from the returned head %s", local, preserved)
	}
	runsBefore := len(h.Runs())
	before := mirrorGuidanceSnapshot(t, h, gateDir, operator)

	var passive string
	for i := 0; i < 2; i++ {
		statusOut, statusErr := h.RunInDir(operator, "axi", "status")
		if statusErr != nil {
			t.Fatalf("axi status: %v\n%s", statusErr, statusOut)
		}
		syncOut, syncErr := h.RunInDir(operator, "axi", "sync")
		if syncErr != nil {
			t.Fatalf("axi sync: %v\n%s", syncErr, syncOut)
		}
		for name, out := range map[string]string{"axi status": statusOut, "axi sync": syncOut} {
			if strings.Contains(out, "code: run_pipeline") {
				t.Fatalf("%s advertised run_pipeline although the planner will refuse:\n%s", name, out)
			}
			for _, want := range []string{"state: custody_returned", "safety: private_mirror_reconciliation_required", "code: check_private_mirror", "no-mistakes axi sync --check"} {
				if !strings.Contains(out, want) {
					t.Fatalf("%s output missing %q:\n%s", name, want, out)
				}
			}
		}
		if i == 0 {
			passive = statusOut + syncOut
		} else if statusOut+syncOut != passive {
			t.Fatalf("passive guidance changed between identical reads:\nfirst:\n%s\nsecond:\n%s", passive, statusOut+syncOut)
		}
		if after := mirrorGuidanceSnapshot(t, h, gateDir, operator); after != before {
			t.Fatalf("passive status changed refs or objects:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	}

	var checked string
	for i := 0; i < 2; i++ {
		checkOut, checkErr := h.RunInDir(operator, "axi", "sync", "--check")
		if checkErr == nil {
			t.Fatalf("sync --check reported success for a head the planner refuses:\n%s", checkOut)
		}
		if strings.Contains(checkOut, "run_pipeline") {
			t.Fatalf("sync --check advertised run_pipeline although the planner refuses:\n%s", checkOut)
		}
		for _, want := range []string{"state: custody_returned", "safety: blocked_private_mirror", "refusing to reconcile private mirror ref refs/heads/feature/mirror-guidance", submitted, "No sync action applies to this state"} {
			if !strings.Contains(checkOut, want) {
				t.Fatalf("sync --check output missing %q:\n%s", want, checkOut)
			}
		}
		if strings.Contains(checkOut, "next_action") {
			t.Fatalf("sync --check offered a next action for a refused head:\n%s", checkOut)
		}
		if i == 0 {
			checked = checkOut
		} else if checkOut != checked {
			t.Fatalf("sync --check changed between identical reads:\nfirst:\n%s\nsecond:\n%s", checked, checkOut)
		}
	}
	if laneHead := strings.TrimSpace(string(mustOutput(t)(h.runGit(ctx, gateDir, "rev-parse", "refs/heads/feature/mirror-guidance")))); laneHead != submitted {
		t.Fatalf("sync --check moved the gate lane to %s", laneHead)
	}
	if tags := strings.TrimSpace(string(mustOutput(t)(h.runGit(ctx, gateDir, "for-each-ref", "refs/tags/no-mistakes-abandoned/", "refs/no-mistakes/fetch/")))); tags != "" {
		t.Fatalf("sync --check left refs behind:\n%s", tags)
	}
	if got := strings.TrimSpace(string(mustOutput(t)(h.runGit(ctx, operator, "rev-parse", "HEAD")))); got != local {
		t.Fatalf("sync --check moved the worktree to %s", got)
	}

	runOut, runErr := h.RunInDir(operator, "axi", "run", "--intent", "ship the deliberate canary revert")
	if runErr == nil || !strings.Contains(runOut, "refusing to reconcile private mirror ref refs/heads/feature/mirror-guidance") {
		t.Fatalf("axi run was not refused by the private mirror planner (err %v):\n%s", runErr, runOut)
	}
	if got := len(h.Runs()); got != runsBefore {
		t.Fatalf("refused axi run created %d run(s)", got-runsBefore)
	}
	if laneHead := strings.TrimSpace(string(mustOutput(t)(h.runGit(ctx, gateDir, "rev-parse", "refs/heads/feature/mirror-guidance")))); laneHead != submitted {
		t.Fatalf("refused axi run moved the gate lane to %s", laneHead)
	}

	if out, gitErr := h.runGit(ctx, operator, "merge", "-s", "ours", "--no-ff", "-m", "supersede the submitted head", submitted); gitErr != nil {
		t.Fatalf("operator ancestry commit: %v\n%s", gitErr, out)
	}
	allowedStatus, statusErr := h.RunInDir(operator, "axi", "status")
	if statusErr != nil {
		t.Fatalf("axi status after ancestry: %v\n%s", statusErr, allowedStatus)
	}
	for _, want := range []string{"safety: custody_returned", "code: run_pipeline", "no-mistakes axi run --intent"} {
		if !strings.Contains(allowedStatus, want) {
			t.Fatalf("status did not offer the allowed run (%q missing):\n%s", want, allowedStatus)
		}
	}
	allowedCheck, checkErr := h.RunInDir(operator, "axi", "sync", "--check")
	if checkErr != nil || !strings.Contains(allowedCheck, "code: run_pipeline") {
		t.Fatalf("sync --check did not offer the allowed run (err %v):\n%s", checkErr, allowedCheck)
	}
	freshOut, err := h.RunInDir(operator, "axi", "run", "--intent", "ship the deliberate canary revert")
	if err != nil || !strings.Contains(freshOut, "gate:") {
		t.Fatalf("allowed fresh run did not start (err %v):\n%s", err, freshOut)
	}
}

func mustOutput(t *testing.T) func([]byte, error) []byte {
	t.Helper()
	return func(out []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
		return out
	}
}

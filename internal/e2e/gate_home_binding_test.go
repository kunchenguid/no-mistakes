//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decoyHome is a second no-mistakes root that never has a daemon: a push that
// misroutes to it can only fail, and any state created under it proves the CLI
// dialed the wrong root.
func decoyHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "decoy-home")
	if err := os.MkdirAll(filepath.Join(home, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestGateHookBindsPushToTheHomeThatOwnsTheGate drives the reported bug end to
// end: git never sets NM_HOME for a hook, so whatever the pushing shell
// exported used to decide which daemon authorized and ran the push. Pushing
// with NM_HOME naming a different root must still reach the daemon that owns
// the gate, and must leave that other root untouched.
func TestGateHookBindsPushToTheHomeThatOwnsTheGate(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/cross-home-push"
	h.CommitChange(branch, "cross-home.txt", "cross home\n", "add cross home file")

	other := decoyHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "push", "no-mistakes", branch)
	cmd.Dir = h.WorkDir
	cmd.Env = mergedEnv(os.Environ(), map[string]string{
		"NM_HOME":             other,
		"GIT_AUTHOR_NAME":     "E2E Test",
		"GIT_AUTHOR_EMAIL":    "e2e@example.com",
		"GIT_COMMITTER_NAME":  "E2E Test",
		"GIT_COMMITTER_EMAIL": "e2e@example.com",
	})
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("push with a foreign NM_HOME exported was rejected: %v\n%s", err, out)
	}
	t.Logf("git push output:\n%s", out)

	run := h.WaitForRun(branch, 90*time.Second)
	t.Logf("owning daemon run: id=%s branch=%s status=%s", run.ID, run.Branch, run.Status)

	// The other root must never have been dialed: no daemon socket, no db.
	for _, name := range []string{"daemon.sock", "no-mistakes.db"} {
		if _, statErr := os.Stat(filepath.Join(other, name)); statErr == nil {
			t.Fatalf("push created %s under the NM_HOME the shell exported; the hook must bind to the gate's own root", name)
		}
	}
}

// TestDaemonRefusesGateFromAnotherHome drives the two hook-invoked CLI legs
// straight at a live daemon with a gate under a different root - what a stale
// pre-binding hook or a hand-run CLI produces - and requires an explicit
// refusal on both, including the admit leg that authorizes the ref mutation.
func TestDaemonRefusesGateFromAnotherHome(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	ownedGate := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if _, err := os.Stat(ownedGate); err != nil {
		t.Fatalf("expected gate at %s: %v", ownedGate, err)
	}
	other := decoyHome(t)
	foreignGate := filepath.Join(other, "repos", h.repoID()+".git")
	if out, err := h.runGit(context.Background(), other, "init", "--bare", foreignGate); err != nil {
		t.Fatalf("init foreign gate: %v\n%s", err, out)
	}
	head := h.WorktreeRefSHA("HEAD")

	legs := []struct {
		name string
		args []string
	}{
		{"admit-push", []string{"daemon", "admit-push", "--gate", foreignGate}},
		{"notify-push", []string{
			"daemon", "notify-push",
			"--gate", foreignGate,
			"--ref", "refs/heads/main",
			"--old", "0000000000000000000000000000000000000000",
			"--new", head,
		}},
	}
	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			out, err := h.RunInDir(h.WorkDir, leg.args...)
			if err == nil {
				t.Fatalf("%s accepted a gate under another home:\n%s", leg.name, out)
			}
			if !strings.Contains(out, "does not belong to this daemon's home") {
				t.Fatalf("%s refusal must name the cause, got:\n%s", leg.name, out)
			}
			t.Logf("%s refusal:\n%s", leg.name, strings.TrimSpace(out))
		})
	}

	// The guard must not cost the ordinary case: this root's own gate is still
	// admitted by the same live daemon.
	if out, err := h.RunInDir(h.WorkDir, "daemon", "admit-push", "--gate", ownedGate); err != nil {
		t.Fatalf("admit-push for the owned gate must still be authorized: %v\n%s", err, out)
	} else {
		t.Logf("owned gate admit-push accepted (no output expected): %q", strings.TrimSpace(out))
	}
}

// TestPushToAMisshapenGateIsRefusedClosed: a gate that is not laid out as
// <home>/repos/<id>.git gives the hook no owning root to derive, and admission
// is a security boundary, so the push must be refused rather than routed to
// whichever daemon the ambient NM_HOME names.
func TestPushToAMisshapenGateIsRefusedClosed(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	ctx := context.Background()

	ownedGate := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	misshapen := filepath.Join(t.TempDir(), "loose-gate.git")
	if out, err := h.runGit(ctx, h.WorkDir, "init", "--bare", misshapen); err != nil {
		t.Fatalf("init misshapen gate: %v\n%s", err, out)
	}
	// The managed hooks as the product rendered them, in a gate whose path
	// carries no owning root.
	for _, hook := range []string{"pre-receive", "post-receive"} {
		src, err := os.ReadFile(filepath.Join(ownedGate, "hooks", hook))
		if err != nil {
			t.Fatalf("read managed %s: %v", hook, err)
		}
		if err := os.WriteFile(filepath.Join(misshapen, "hooks", hook), src, 0o755); err != nil {
			t.Fatalf("install %s: %v", hook, err)
		}
	}

	branch := "feature/misshapen-gate"
	h.CommitChange(branch, "misshapen.txt", "misshapen\n", "add misshapen file")
	out, err := h.runGit(ctx, h.WorkDir, "push", misshapen, branch)
	if err == nil {
		t.Fatalf("push to a gate with no derivable home was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot derive the gate home") {
		t.Fatalf("refusal must name the cause, got:\n%s", out)
	}
	t.Logf("refused push:\n%s", out)
	if refs, refErr := h.runGit(ctx, misshapen, "for-each-ref", "--format=%(refname)"); refErr != nil {
		t.Fatalf("list refs: %v\n%s", refErr, refs)
	} else if strings.Contains(string(refs), branch) {
		t.Fatalf("refused push still mutated the gate refs:\n%s", refs)
	}
}

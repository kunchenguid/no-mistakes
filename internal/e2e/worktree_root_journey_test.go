//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestWorktreeRootJourney walks the whole worktree_roots feature the way an
// operator reaches it, with nothing about the placement stubbed:
//
//   - `no-mistakes init --worktree-root <dir>` refuses the directories the
//     daemon refuses to start on, so the entry it prints is one that can be
//     pasted.
//   - the entry it prints is pasted into the real global config verbatim, and
//     the daemon restarts on it.
//   - a real `git push no-mistakes` run is then created in that directory:
//     every pipeline agent runs with <dir>/<run id> as its cwd, which is the
//     whole point of the setting (mise/direnv resolve by path ancestry), and
//     the run records that directory.
//   - the default placement under NM_HOME is never used for that run, and when
//     the run ends only its own directory is gone - the operator's own files in
//     that directory are untouched.
//
// The unit tests own each seam; this is the wiring check that the printed
// guidance, the config, run creation, the agent cwd, and cleanup all agree on
// one directory.
func TestWorktreeRootJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	// The operator's own directory: outside NM_HOME and outside every
	// checkout, holding the kind of directory-scoped toolchain config that
	// motivates the setting.
	runsRoot := filepath.Join(t.TempDir(), "my-repo-runs")
	if err := os.MkdirAll(runsRoot, 0o755); err != nil {
		t.Fatalf("mkdir runs root: %v", err)
	}
	operatorFile := filepath.Join(runsRoot, "mise.local.toml")
	if err := os.WriteFile(operatorFile, []byte("[tools]\ngo = \"1.25\"\n"), 0o644); err != nil {
		t.Fatalf("write operator file: %v", err)
	}

	// 1. Refusals come before anything is registered, so the operator can
	// still pick another directory.
	insideCheckout := filepath.Join(h.WorkDir, "runs")
	out, err := h.RunInDir(h.WorkDir, "init", "--worktree-root", insideCheckout)
	if err == nil {
		t.Fatalf("init accepted a worktree root inside the checkout:\n%s", out)
	}
	if !strings.Contains(out, "blocks branch synchronization") && !strings.Contains(out, "untracked run worktree") {
		t.Errorf("refusal for a root inside the checkout does not say why:\n%s", out)
	}
	insideNMHome := filepath.Join(h.NMHome, "worktrees", "runs")
	out, err = h.RunInDir(h.WorkDir, "init", "--worktree-root", insideNMHome)
	if err == nil {
		t.Fatalf("init accepted a worktree root inside NM_HOME:\n%s", out)
	}
	if !strings.Contains(out, "state directory") {
		t.Errorf("refusal for a root inside NM_HOME does not say why:\n%s", out)
	}

	// 2. init prints the entry that places this checkout's runs.
	initOut, err := h.RunInDir(h.WorkDir, "init", "--worktree-root", runsRoot)
	if err != nil {
		t.Fatalf("nm init --worktree-root: %v\n%s", err, initOut)
	}
	t.Logf("--- no-mistakes init --worktree-root %s ---\n%s", runsRoot, initOut)
	entryLine := worktreeRootEntryFromGuidance(t, initOut, runsRoot)

	// 3. The operator pastes it. The config is hand-maintained, so this is
	// the real edit: init never writes it.
	configPath := filepath.Join(h.NMHome, "config.yaml")
	existing, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	pasted := string(existing) + "worktree_roots:\n" + entryLine + "\n"
	if err := os.WriteFile(configPath, []byte(pasted), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	// The daemon must accept the pasted config, which is the claim that makes
	// the printout safe to paste at all: every command starts the daemon.
	if out, err := h.Run("daemon", "restart"); err != nil {
		t.Fatalf("daemon restart on the pasted config: %v\n%s", err, out)
	}

	// 4. A real push through the gate.
	branch := "feature/worktree-root"
	h.CommitChange(branch, "placed.txt", "placed run worktrees\n", "add placed.txt")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed; error = %v", run.Status, run.Error)
	}

	wantDir := filepath.Join(runsRoot, run.ID)

	// 5. Every pipeline agent ran inside the configured directory. This is
	// the end-user consequence: a run worktree under it inherits whatever the
	// directory configures, which a worktree under NM_HOME never can.
	invocations := h.AgentInvocations()
	if len(invocations) == 0 {
		t.Fatal("no agent invocations recorded; the run cannot have exercised placement")
	}
	want := canonicalForCompare(t, wantDir)
	for i, inv := range invocations {
		if got := canonicalForCompare(t, inv.CWD); got != want {
			t.Errorf("agent invocation %d ran in cwd %q, want the configured placement %q", i, inv.CWD, want)
		}
	}
	t.Logf("all %d pipeline agent invocations ran in %s", len(invocations), invocations[0].CWD)

	// 6. The run recorded that directory, which is what every later consumer
	// (resume, step diff, cleanup, reaping, eject) reads back.
	recorded := readRunWorktreeDir(t, h.NMHome, run.ID)
	if canonicalForCompare(t, recorded) != want {
		t.Errorf("runs.worktree_dir = %q, want %q", recorded, want)
	}

	// 7. The default placement was never used for this run.
	defaultDir := paths.WithRoot(h.NMHome).WorktreeDir(h.repoID(), run.ID)
	if _, err := os.Stat(defaultDir); err == nil {
		t.Errorf("default placement %q exists; the configured root was not used", defaultDir)
	}
	t.Logf("run %s recorded worktree_dir %s; the default placement %s was never created",
		run.ID, recorded, defaultDir)

	// 8. Cleanup removed the run's own directory and nothing else in the
	// operator's directory. Cleanup runs after the run reaches its terminal
	// status, so this waits for it rather than sampling once.
	waitForRemoval(t, wantDir, 30*time.Second)
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		t.Fatalf("read runs root: %v", err)
	}
	var leftovers []string
	for _, entry := range entries {
		if entry.Name() != filepath.Base(operatorFile) {
			leftovers = append(leftovers, entry.Name())
		}
	}
	if len(leftovers) > 0 {
		t.Errorf("configured root holds unexpected entries after the run: %v", leftovers)
	}
	if _, err := os.Stat(operatorFile); err != nil {
		t.Errorf("operator's own file in the configured root did not survive: %v", err)
	}
	t.Logf("after the run, %s holds only the operator's own %s", runsRoot, filepath.Base(operatorFile))
}

// waitForRemoval waits for the daemon's run cleanup to remove dir.
func waitForRemoval(t *testing.T, dir string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("run worktree %q was still present %v after the run finished", dir, timeout)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// worktreeRootEntryFromGuidance reads the config entry line init printed,
// exactly as it must be pasted. It is read out of the command's own output
// rather than reconstructed, because the point of the printout is that pasting
// what it says works.
func worktreeRootEntryFromGuidance(t *testing.T, out, root string) string {
	t.Helper()
	if !strings.Contains(out, "worktree_roots:") {
		t.Fatalf("init did not print the worktree_roots key:\n%s", out)
	}
	ansi := regexp.MustCompile("\x1b\\[[0-9;]*m")
	for _, line := range strings.Split(out, "\n") {
		line = ansi.ReplaceAllString(line, "")
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimSpace(line)
		candidateKey, candidateValue, ok := strings.Cut(trimmed, ": ")
		if !ok || candidateKey == "" {
			continue
		}
		if canonicalForCompare(t, candidateValue) != canonicalForCompare(t, root) {
			continue
		}
		if canonicalForCompare(t, candidateKey) == canonicalForCompare(t, root) {
			continue
		}
		// The entry line keeps the indentation init printed it under the
		// key with, minus the two columns of output framing.
		return strings.TrimPrefix(line, "  ")
	}
	t.Fatalf("init printed no worktree_roots entry naming %q:\n%s", root, out)
	return ""
}

func readRunWorktreeDir(t *testing.T, nmHome, runID string) string {
	t.Helper()
	database, err := db.Open(paths.WithRoot(nmHome).DB())
	if err != nil {
		t.Fatalf("open e2e db: %v", err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	if run == nil {
		t.Fatalf("run %s not in db", runID)
	}
	return run.WorktreePath()
}

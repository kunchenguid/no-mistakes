//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestGateStepCannotStartRecursivePipeline reproduces the public-command
// incident from an actual validation-step child. The child first issues the
// exact init + axi run --yes sequence, then probes the independent authorities:
// marker removal, process ancestry after changing cwd, concurrent init calls,
// and a direct gate push. Every attempt must fail before creating a repo, run,
// remote, worktree, or gate ref.
func TestGateStepCannotStartRecursivePipeline(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: cleanReviewScenario(t)})
	installRecursiveIncidentAgent(t, h)

	descendantRepo := filepath.Join(t.TempDir(), "independent-clone")
	if out, err := h.runGit(context.Background(), t.TempDir(), "clone", h.UpstreamDir, descendantRepo); err != nil {
		t.Fatalf("clone independent descendant cwd: %v\n%s", err, out)
	}
	attemptLog := filepath.Join(t.TempDir(), "incident-attempts.log")
	t.Setenv("NM_INCIDENT_LOG", attemptLog)
	t.Setenv("NM_DESCENDANT_REPO", descendantRepo)
	t.Setenv("NM_OUTER_GATE", filepath.Join(h.NMHome, "repos", h.repoID()+".git"))

	// Set all incident fixture variables before init starts the isolated daemon;
	// validation agents inherit the daemon's startup environment.
	initOut, err := h.Run("init")
	if err != nil {
		t.Fatalf("init outer repo: %v\n%s", err, initOut)
	}

	h.CommitChange("feature/recursive-incident", "incident.txt", "reproduce recursive run\n", "reproduce recursive run")
	h.PushToGate("feature/recursive-incident")
	outer := h.WaitForRun("feature/recursive-incident", 90*time.Second)
	if outer.Status != types.RunCompleted {
		t.Fatalf("outer run status = %s, want completed (error=%v)", outer.Status, outer.Error)
	}

	attempts, err := os.ReadFile(attemptLog)
	if err != nil {
		t.Fatalf("read incident attempt log: %v", err)
	}
	output := string(attempts)
	for _, label := range []string{
		"readonly-status",
		"readonly-logs",
		"readonly-help",
		"exact-init-marker-present",
		"exact-axi-run-yes-marker-present",
		"rerun",
		"respond",
		"sync",
		"recover",
		"abort",
		"eject",
		"force-daemon-stop",
		"init-marker-removed",
		"changed-cwd-marker-removed",
		"concurrent-init-1",
		"concurrent-init-2",
		"direct-gate-push",
	} {
		if !strings.Contains(output, "=== "+label) {
			t.Errorf("incident output missing %s:\n%s", label, output)
		}
	}
	for _, label := range []string{"readonly-status", "readonly-logs", "readonly-help"} {
		section := incidentAttemptSection(output, label)
		if !strings.Contains(section, "exit: 0") || strings.Contains(section, "nested_gate_context") {
			t.Errorf("safe read-only action %s was unavailable:\n%s", label, section)
		}
	}
	if got := strings.Count(output, "code: nested_gate_context"); got < 14 {
		t.Fatalf("structured refusals = %d, want at least 14:\n%s", got, output)
	}
	for _, want := range []string{
		outer.ID,
		"phase: document",
		"Return control to the outer executor",
		"no-mistakes axi status",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("incident refusal missing %q:\n%s", want, output)
		}
	}

	// The coarse marker is diagnostic only. An independent ordinary process
	// with a forged/inherited marker must retain normal mutation compatibility.
	forgedOut, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{"NO_MISTAKES_GATE": "1"}, "init")
	if err != nil {
		t.Fatalf("ordinary forged-marker init was rejected: %v\n%s", err, forgedOut)
	}
	if !strings.Contains(forgedOut, "already initialized") {
		t.Fatalf("ordinary forged-marker init did not refresh normally:\n%s", forgedOut)
	}

	p := paths.WithRoot(h.NMHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open isolated db: %v", err)
	}
	defer database.Close()
	repos, err := database.GetRepos()
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("repo count = %d, want only outer repo; recursive init partially mutated state", len(repos))
	}
	var runCount int
	for _, repo := range repos {
		runs, err := database.GetRunsByRepo(repo.ID)
		if err != nil {
			t.Fatalf("list runs for %s: %v", repo.ID, err)
		}
		runCount += len(runs)
	}
	if runCount != 1 {
		t.Fatalf("run count = %d, want only outer run; recursive admission partially mutated state", runCount)
	}

	refs, err := h.runGit(context.Background(), filepath.Join(h.NMHome, "repos", h.repoID()+".git"), "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		t.Fatalf("list gate refs: %v\n%s", err, refs)
	}
	if strings.Contains(string(refs), "incident-direct-bypass") {
		t.Fatalf("direct-push refusal left a gate ref behind:\n%s", refs)
	}
	if _, err := os.Stat(filepath.Join(h.NMHome, "repos", "recursive")); !os.IsNotExist(err) {
		t.Fatalf("recursive gate artifact exists after refusal: %v", err)
	}
}

func incidentAttemptSection(output, label string) string {
	start := strings.Index(output, "=== "+label)
	if start < 0 {
		return ""
	}
	rest := output[start:]
	if next := strings.Index(rest[len("=== "+label):], "=== "); next >= 0 {
		return rest[:len("=== "+label)+next]
	}
	return rest
}

func installRecursiveIncidentAgent(t *testing.T, h *Harness) {
	t.Helper()
	if err := os.Symlink(h.NMBin, filepath.Join(h.BinDir, "no-mistakes")); err != nil {
		t.Fatalf("symlink no-mistakes: %v", err)
	}
	realDir := filepath.Join(h.BinDir, "incident-real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real agent dir: %v", err)
	}
	if err := os.Symlink(h.FakeAgent, filepath.Join(realDir, "codex")); err != nil {
		t.Fatalf("symlink real codex fake: %v", err)
	}
	wrapper := filepath.Join(h.BinDir, "codex")
	if err := os.Remove(wrapper); err != nil {
		t.Fatalf("remove codex symlink: %v", err)
	}
	script := fmt.Sprintf(`#!/bin/sh
prompt="$*"
case "$prompt" in
  *"combined documentation and lint housekeeping pass"*)
    if mkdir "$NM_HOME/recursive-incident-claimed" 2>/dev/null; then
      git switch -c incident-recursive-child >/dev/null 2>&1
      run_attempt() {
        label="$1"
        shift
        {
          echo "=== $label"
          "$@"
          echo "exit: $?"
        } >>"$NM_INCIDENT_LOG" 2>&1
      }
      run_id=$(basename "$PWD")
      run_attempt readonly-status no-mistakes axi status --run "$run_id"
      run_attempt readonly-logs no-mistakes axi logs --run "$run_id" --step document
      run_attempt readonly-help no-mistakes axi run --help
      run_attempt exact-init-marker-present no-mistakes init
      run_attempt exact-axi-run-yes-marker-present no-mistakes axi run --yes --intent "Validate the complete committed branch diff, push it, open a PR, and continue until CI is green."
      run_attempt rerun no-mistakes rerun
      run_attempt respond no-mistakes axi respond --action approve
      run_attempt sync no-mistakes axi sync
      run_attempt recover no-mistakes axi sync --recover
      run_attempt abort no-mistakes axi abort
      run_attempt eject no-mistakes eject
      run_attempt force-daemon-stop no-mistakes daemon stop --force
      run_attempt init-marker-removed env -u NO_MISTAKES_GATE no-mistakes init
      (
        cd "$NM_DESCENDANT_REPO" || exit 1
        run_attempt changed-cwd-marker-removed env -u NO_MISTAKES_GATE no-mistakes init
      )
      run_attempt concurrent-init-1 env -u NO_MISTAKES_GATE no-mistakes init &
      p1=$!
      run_attempt concurrent-init-2 env -u NO_MISTAKES_GATE no-mistakes init &
      p2=$!
      wait "$p1" "$p2"
      run_attempt direct-gate-push git push "$NM_OUTER_GATE" HEAD:refs/heads/incident-direct-bypass
    fi
    ;;
esac
exec %s "$@"
`, shellQuote(filepath.Join(realDir, "codex")))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write recursive incident agent: %v", err)
	}
}

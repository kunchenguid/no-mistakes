//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTestEvidenceLivesUnderAppRootNotSharedTemp is the end-to-end guard for
// the whole relocation, and the test that would have caught the original
// defect.
//
// The daemon used to build its evidence root as os.TempDir()/no-mistakes-evidence.
// Under a service unit TMPDIR is unset, so on Linux that resolved to the shared
// /tmp - a systemd tmpfs on current Ubuntu, meaning every screenshot the test
// step gathered was held in RAM. Nothing in this program ever removed it
// either: the directory was a fixed name that accumulated one subdirectory per
// run until an OS timer or a reboot happened to clear it.
//
// This drives a real gate push through the real daemon and asserts the three
// things that together fix that, at the boundary a user actually experiences:
// the run names an evidence directory under NM_HOME, every agent it launches is
// steered to that same directory, and the shared temp location is never touched.
func TestTestEvidenceLivesUnderAppRootNotSharedTemp(t *testing.T) {
	// The legacy root is a shared machine-wide path that unrelated activity may
	// already have created, so the assertion below is scoped to this run's own
	// directory rather than to the root's existence. Overriding TMPDIR here is
	// not an option: the harness builds its binaries under it once per test
	// process, so a per-test temp directory would delete them out from under
	// the next test.
	legacyRoot := filepath.Join(os.TempDir(), "no-mistakes-evidence")

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/evidence-location"
	h.CommitChange(branch, "hello.txt", "hello evidence\n", "add a file to validate")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	appEvidenceRoot := filepath.Join(h.NMHome, "evidence")
	wantRunDir := filepath.Join(appEvidenceRoot, run.ID)

	// The test step tells the agent exactly where to write evidence. That
	// instruction is the run's own statement of where evidence goes.
	testPrompt := findInvocationContaining(h.AgentInvocations(), "You are validating a code change by testing it")
	if testPrompt == "" {
		t.Fatal("the test step never ran, so this test proves nothing")
	}
	if !strings.Contains(testPrompt, wantRunDir) {
		t.Fatalf("test step did not point the agent at %s:\n%s", wantRunDir, testPrompt)
	}
	if strings.Contains(testPrompt, legacyRoot) {
		t.Fatalf("test step still points the agent at the shared temp directory %s:\n%s", legacyRoot, testPrompt)
	}

	// Every pipeline agent carries the steering preamble, which permits exactly
	// one out-of-worktree destination. It must name the same root - these two
	// used to be independent copies of the path.
	invocations := h.AgentInvocations()
	if len(invocations) == 0 {
		t.Fatal("no agent invocations recorded")
	}
	for _, inv := range invocations {
		if !strings.Contains(inv.Prompt, "Workspace boundary (important)") {
			continue
		}
		if !strings.Contains(inv.Prompt, appEvidenceRoot) {
			t.Fatalf("steering preamble does not name the app-root evidence directory %s:\n%s", appEvidenceRoot, inv.Prompt)
		}
		if strings.Contains(inv.Prompt, legacyRoot) {
			t.Fatalf("steering preamble still names the shared temp directory %s:\n%s", legacyRoot, inv.Prompt)
		}
	}

	// This run must have written nothing into the shared temp location.
	legacyRunDir := filepath.Join(legacyRoot, run.ID)
	if _, err := os.Stat(legacyRunDir); !os.IsNotExist(err) {
		t.Fatalf("run %s created evidence under the shared temp directory %s (stat err = %v)", run.ID, legacyRunDir, err)
	}

	// And the app-root location is where it really went: the daemon created it
	// during the run, then cleaned it up because this scenario reports no
	// artifacts. Either state proves the location; a directory under the
	// legacy root would not.
	if _, err := os.Stat(appEvidenceRoot); err != nil && !os.IsNotExist(err) {
		t.Fatalf("app-root evidence directory %s is unusable: %v", appEvidenceRoot, err)
	}
}

// TestRunCleanupLeavesNoEmptyEvidenceDirectory is the litter half of the fix,
// end to end. This scenario's agent reports no artifacts, so the run's evidence
// directory is created and then left empty - which before the fix meant one
// permanent directory per run forever. On a real machine 823 of 876 accumulated
// directories were exactly this.
func TestRunCleanupLeavesNoEmptyEvidenceDirectory(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const branch = "feature/evidence-litter"
	h.CommitChange(branch, "hello.txt", "hello litter\n", "add a file to validate")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	runDir := filepath.Join(h.NMHome, "evidence", run.ID)
	// Cleanup runs in the run goroutine's defer, just after the status the
	// wait above observed, so give it a moment to land.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := os.Stat(runDir)
		if os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			entries, readErr := os.ReadDir(runDir)
			t.Fatalf("empty evidence directory %s survived the run (entries=%v, readErr=%v)", runDir, entries, readErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type gateContextOutput struct {
	SchemaVersion int    `json:"schema_version"`
	Available     bool   `json:"available"`
	Reason        string `json:"reason"`
	types.GateRunContext
}

func TestCustomGateRunContext(t *testing.T) {
	scenario := cleanReviewScenario(t)
	data, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "actions:\n", `actions:
  - match: 'repository gate "local-ci"'
    edits:
      - path: gate-state.txt
        old: "pending\n"
        new: "fixed\n"
    structured:
      summary: satisfy local CI
`, 1))
	if err := os.WriteFile(scenario, data, 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if err := os.Symlink(h.NMBin, filepath.Join(h.BinDir, "no-mistakes")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_CONTEXT_ORDINARY", h.WorkDir)
	h.CommitChange("main", "gate-state.txt", "pending\n", "add local CI requirement")
	h.CommitChange("main", "scripts/local-ci.sh", `#!/bin/sh
log="$NM_HOME/context-$(cat case.txt)"
no-mistakes init > "$log.control" 2>&1
no-mistakes axi context > "$log.toon"
no-mistakes axi context --json >> "$log.json"
sh -c 'unset NO_MISTAKES_GATE; no-mistakes axi context --json' >> "$log.json"
(cd "$NM_CONTEXT_ORDINARY"; no-mistakes axi context --json) > "$log.wrong-copy"
test "$(cat gate-state.txt)" = fixed
`, "add local CI check")
	pushMainRepoConfig(t, h, `allow_repo_commands: true
gates:
  - name: local-ci
    after: test
    command: sh scripts/local-ci.sh
`)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	var previousID string
	for _, name := range []string{"first", "second"} {
		branch := "feature/context-" + name
		// Each branch starts at main, so each run needs its own authorized fix.
		h.CommitChange(branch, "case.txt", name, "exercise local CI")
		h.PushToGate(branch)
		step := types.StepName("gate.test.local-ci")
		parked := waitForStepStatus(t, h, branch, step, types.StepStatusAwaitingApproval, 90*time.Second)
		if parked.ID == previousID {
			t.Fatal("a new run reused the previous run ID")
		}
		previousID = parked.ID
		prefix := filepath.Join(h.NMHome, "context-"+name)
		control := readContextFile(t, prefix+".control")
		t.Logf("gate shell pipeline-control comparison:\n%s", control)
		for _, want := range []string{"code: nested_gate_context", parked.ID, "phase: gate.test.local-ci"} {
			if !strings.Contains(control, want) {
				t.Errorf("control refusal lacks %q", want)
			}
		}
		if out := readContextFile(t, prefix+".toon"); !strings.Contains(out, "available: true") {
			t.Fatalf("gate shell TOON context: %s", out)
		}
		initial := readGateContexts(t, prefix+".json")
		if len(initial) != 2 {
			t.Fatalf("initial direct/descendant contexts = %d, want 2", len(initial))
		}
		for _, got := range initial {
			assertAvailableGateContext(t, got, parked.ID, h.repoID(), step)
		}
		assertUnavailableContext(t, readContextFile(t, prefix+".wrong-copy"))
		// Topology alone, even in the exact run worktree, never proves ancestry.
		out, err := h.RunInDirWithEnv(initial[0].Worktree, map[string]string{"NO_MISTAKES_GATE": "1"}, "axi", "context", "--json")
		if err == nil {
			t.Fatal("independent caller with a forged marker obtained context")
		}
		assertUnavailableContext(t, out)
		fw := h.AddWorktree(branch)
		fixOut, err := h.RunInDir(fw, "axi", "respond", "--action", "fix", "--findings", firstFindingID(t, parked, step))
		if err != nil {
			t.Fatalf("authorize fixture fix: %v\n%s", err, fixOut)
		}
		final := h.WaitForRun(branch, 90*time.Second)
		if final.Status != types.RunCompleted {
			t.Fatalf("run status %s, error %v", final.Status, final.Error)
		}
		after := readGateContexts(t, prefix+".json")
		if len(after) != 4 {
			t.Fatalf("contexts after fix = %d, want 4", len(after))
		}
		for _, got := range after {
			assertAvailableGateContext(t, got, parked.ID, h.repoID(), step)
			if got.GateRunContext != initial[0].GateRunContext {
				t.Fatalf("same-run fix changed scope: %+v vs %+v", got, initial[0])
			}
		}
	}
}

func readContextFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readGateContexts(t *testing.T, path string) []gateContextOutput {
	t.Helper()
	var out []gateContextOutput
	for _, line := range strings.Split(strings.TrimSpace(readContextFile(t, path)), "\n") {
		var got gateContextOutput
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("decode context %q: %v", line, err)
		}
		out = append(out, got)
	}
	return out
}

func assertAvailableGateContext(t *testing.T, got gateContextOutput, run, repo string, phase types.StepName) {
	t.Helper()
	if got.SchemaVersion != 1 || !got.Available || got.RunID != run || got.RepoID != repo || got.Phase != phase || !filepath.IsAbs(got.Worktree) {
		t.Fatalf("unexpected context: %+v (want %s/%s/%s)", got, run, repo, phase)
	}
}

func assertUnavailableContext(t *testing.T, text string) {
	t.Helper()
	var got gateContextOutput
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode unavailable context %q: %v", text, err)
	}
	if got.SchemaVersion != 1 || got.Available || got.Reason == "" || got.GateRunContext != (types.GateRunContext{}) {
		t.Fatalf("unavailable context exposed an identity: %+v", got)
	}
}

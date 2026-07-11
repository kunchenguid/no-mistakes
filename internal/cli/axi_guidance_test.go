package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// canonicalStaleMonitorPhrases are the load-bearing claims of the corrected
// "PR fell behind / conflicted after checks pass" guidance: the live CI monitor
// auto-rebases and re-pushes such a PR, so the agent runs no command and never
// hand-rebases, and `no-mistakes rerun` is only the dead-monitor recovery.
var canonicalStaleMonitorPhrases = []string{
	"never hand-rebase",
	"re-pushes",
	"no-mistakes rerun",
}

var canonicalPreserveGateFixPhrases = []string{
	"no-mistakes axi run --intent",
	"Never abort-and-restart",
	"prior gate-fix commits",
	"already-resolved findings do not re-surface",
}

// TestStaleMonitorRecoveryInvariant_SyncedAcrossSurfaces guards the narrow
// recovery invariant repeated outside the skill and live axi guidance.
// The agents guide carries these pinned phrases instead of duplicating the
// complete agent-driving workflow.
func TestStaleMonitorRecoveryInvariant_SyncedAcrossSurfaces(t *testing.T) {
	surfaces := map[string]string{
		"skill body":      skill.Markdown(),
		"agents guide":    readAgentsGuide(t),
		"axi help string": staleMonitorGuidance,
	}
	for name, content := range surfaces {
		for _, phrase := range canonicalStaleMonitorPhrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing the canonical stale-monitor guidance phrase %q", name, phrase)
			}
		}
	}

	// The discarded wrong framing must not creep back into any surface.
	for name, content := range surfaces {
		if strings.Contains(content, "rebase step integrates the latest") {
			t.Errorf("%s still carries the discarded 'rebase step integrates the latest default branch' wording", name)
		}
	}
}

// TestStaleMonitorGuidance_InChecksPassedOutput ensures the guidance reaches the
// agent at its point of use: the `checks-passed` axi output, where the agent
// decides what to do about the still-monitored PR.
func TestStaleMonitorGuidance_InChecksPassedOutput(t *testing.T) {
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  types.RunRunning, // not terminal: daemon keeps monitoring until merge
		HeadSHA: "abcdef1234567890",
		PRURL:   strptr("https://github.com/user/repo/pull/42"),
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepCI, Status: types.StepStatusRunning},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := renderDriveResult(cmd, run, true); err != nil {
		t.Fatalf("checks-passed must exit 0, got error: %v", err)
	}

	got := out.String()
	for _, phrase := range canonicalStaleMonitorPhrases {
		if !strings.Contains(got, phrase) {
			t.Errorf("checks-passed output missing stale-monitor guidance phrase %q in:\n%s", phrase, got)
		}
	}
}

func TestPreserveGateFixRecoveryInvariant_SyncedAcrossSurfaces(t *testing.T) {
	surfaces := map[string]string{
		"skill body":       skill.Markdown(),
		"agents guide":     readAgentsGuide(t),
		"axi run help":     newAxiRunCmd().Long,
		"axi respond help": newAxiRespondCmd().Long,
		"axi abort help":   newAxiAbortCmd().Long,
	}
	for name, content := range surfaces {
		for _, phrase := range canonicalPreserveGateFixPhrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing the canonical preserve-gate-fix guidance phrase %q", name, phrase)
			}
		}
	}
}

func TestAgentsGuide_PointsToCurrentDrivingGuidance(t *testing.T) {
	content := readAgentsGuide(t)

	required := []string{
		"[installable `/no-mistakes` skill](https://github.com/kunchenguid/no-mistakes/blob/main/skills/no-mistakes/SKILL.md)",
		"[AXI command reference](/no-mistakes/reference/cli/#no-mistakes-axi)",
		"`no-mistakes axi run --help`",
	}
	for _, phrase := range required {
		if !strings.Contains(content, phrase) {
			t.Errorf("agents guide is missing source-of-truth pointer %q", phrase)
		}
	}

	forbidden := []string{
		"```sh",
		"Agents can also call",
		"Before starting validation",
		"A long-running `axi run`",
		"ACP",
		"acpx",
	}
	for _, phrase := range forbidden {
		if strings.Contains(content, phrase) {
			t.Errorf("agents guide duplicates operational or legacy guidance %q", phrase)
		}
	}
}

func TestPreserveGateFixGuidance_InPointOfUseOutputs(t *testing.T) {
	gate := stepView{
		Name:   "review",
		Status: "awaiting_approval",
		FindingsJSON: findingsJSON(t, []types.Finding{
			{ID: "review-1", Severity: "warning", File: "main.go", Action: types.ActionAskUser, Description: "calls os.Exit"},
		}, "1 blocking issue"),
	}
	surfaces := map[string]string{
		"gate output":          axiDoc(gateFields(gate)...),
		"checks-passed output": renderDriveResultForGuidanceTest(t, true, types.RunRunning),
		"failed output":        renderDriveResultForGuidanceTest(t, false, types.RunFailed),
	}
	for name, content := range surfaces {
		for _, phrase := range canonicalPreserveGateFixPhrases {
			if !strings.Contains(content, phrase) {
				t.Errorf("%s is missing the canonical preserve-gate-fix guidance phrase %q in:\n%s", name, phrase, content)
			}
		}
	}
}

func renderDriveResultForGuidanceTest(t *testing.T, ciReady bool, status types.RunStatus) string {
	t.Helper()
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  status,
		HeadSHA: "abcdef1234567890",
		PRURL:   strptr("https://github.com/user/repo/pull/42"),
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepCI, Status: types.StepStatusRunning},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := renderDriveResult(cmd, run, ciReady)
	var exit *exitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("renderDriveResult returned unexpected error: %v", err)
	}
	return out.String()
}

func readAgentsGuide(t *testing.T) string {
	t.Helper()
	// internal/cli -> repo root is two levels up.
	path := filepath.Join("..", "..", "docs", "src", "content", "docs", "guides", "agents.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agents guide %s: %v", path, err)
	}
	return string(data)
}

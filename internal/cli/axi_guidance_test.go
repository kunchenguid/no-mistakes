package cli

import (
	"bytes"
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

// TestStaleMonitorGuidance_SyncedAcrossSurfaces guards the repo invariant that
// agent-driving guidance stays in sync across its three surfaces: the skill
// body, the published agents guide, and the live axi help string. The earlier
// wrong wording (telling agents to re-run a stale PR with `axi run`) shipped to
// only one surface; this keeps the corrected guidance present on all three.
func TestStaleMonitorGuidance_SyncedAcrossSurfaces(t *testing.T) {
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

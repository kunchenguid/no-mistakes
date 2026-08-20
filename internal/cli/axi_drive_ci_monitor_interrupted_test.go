package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRenderDriveResult_CIMonitorInterrupted exercises the agent-facing
// `axi drive` surface for a run recovered as RunCIMonitorInterrupted (issue
// #361): the daemon restarted while monitoring CI for an already-open PR, so
// the run is a distinct, non-failure terminal outcome that keeps the PR intact.
func TestRenderDriveResult_CIMonitorInterrupted(t *testing.T) {
	if got := outcomeFor(string(types.RunCIMonitorInterrupted)); got != "ci-monitor-interrupted" {
		t.Fatalf("outcomeFor(%q) = %q, want %q", types.RunCIMonitorInterrupted, got, "ci-monitor-interrupted")
	}

	prURL := "https://github.com/user/repo/pull/374"
	reason := types.RunCIMonitorInterruptedReason
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "fix/361-ci-monitor-failed-on-daemon-restart",
		Status:  types.RunCIMonitorInterrupted,
		HeadSHA: "abcdef1234567890",
		Error:   &reason,
		PRURL:   &prURL,
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepPR, Status: types.StepStatusCompleted},
			{StepName: types.StepCI, Status: types.StepStatusSkipped},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	// A non-failure terminal outcome must NOT return a non-zero exit error the
	// way RunFailed does; the PR is intact.
	if err := renderDriveResult(cmd, run, false); err != nil {
		var exit *exitError
		if errors.As(err, &exit) {
			t.Fatalf("interrupted CI monitor must not exit non-zero (PR remains open); got exit code %d", exit.code)
		}
		t.Fatalf("renderDriveResult returned unexpected error: %v", err)
	}

	rendered := out.String()
	t.Logf("axi drive output for a CI-monitor-interrupted run:\n%s", rendered)

	for _, want := range []string{
		"ci-monitor-interrupted",
		"The daemon restarted while monitoring CI; the PR remains open and was not marked failed.",
		"Open the PR: " + prURL,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered drive output missing %q in:\n%s", want, rendered)
		}
	}
	// It must not shove the ordinary failure/gate guidance at the agent.
	if strings.Contains(rendered, preserveGateFixCommitsGuidance) {
		t.Errorf("interrupted CI monitor output leaked ordinary gate-fix guidance:\n%s", rendered)
	}
}

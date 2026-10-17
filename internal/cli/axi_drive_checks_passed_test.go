package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// TestRenderDriveResult_TerminalChecksPassed is the default green shape: the
// run already reached its verdict and released the branch, so it reports the
// same outcome word with the released contract - and specifically not the
// still-monitoring guidance, which would tell an agent to wait for a monitor
// that no longer exists.
func TestRenderDriveResult_TerminalChecksPassed(t *testing.T) {
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  types.RunChecksPassed,
		HeadSHA: "abcdef1234567890",
		PRURL:   strptr("https://github.com/user/repo/pull/42"),
		CIReady: true,
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepCI, Status: types.StepStatusCompleted},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := renderDriveResult(cmd, run, false); err != nil {
		t.Fatalf("a checks_passed run must exit 0, got error: %v", err)
	}

	got := out.String()
	for _, want := range append([]string{
		"outcome: checks-passed",
		"CI checks passed",
		"https://github.com/user/repo/pull/42",
		"Summarize this pipeline run for the user",
	}, canonicalReleasedChecksPassedPhrases...) {
		if !strings.Contains(got, want) {
			t.Errorf("terminal checks-passed output missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "outcome: passed\n") {
		t.Errorf("checks-passed must not report a terminal passed outcome:\n%s", got)
	}
	// The released run must not carry the still-monitoring contract, whose
	// first instruction is that the CI monitor will rebase and re-push.
	if strings.Contains(got, "the CI monitor rebases onto the base") {
		t.Errorf("a released run must not claim a live monitor:\n%s", got)
	}
}

// TestOutcomeFor_ChecksPassedStatus pins the outcome vocabulary: a run that
// ended at checks_passed keeps reporting checks-passed, and stays distinct from
// an ordinary completion (a merged or otherwise finished PR).
func TestOutcomeFor_ChecksPassedStatus(t *testing.T) {
	if got := outcomeFor(string(types.RunChecksPassed)); got != "checks-passed" {
		t.Fatalf("outcomeFor(%s) = %q, want checks-passed", types.RunChecksPassed, got)
	}
	if got := outcomeFor(string(types.RunCompleted)); got != "passed" {
		t.Fatalf("outcomeFor(%s) = %q, want passed", types.RunCompleted, got)
	}
}

func TestDriveRun_ReleaseAndWatchGuidance(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "watch"}[watch], func(t *testing.T) {
			firstEvents := make(chan ipc.Event)
			close(firstEvents)
			secondEvents := make(chan ipc.Event)
			close(secondEvents)
			source := &scriptedRunStateSource{
				subscriptions: []scriptedSubscription{{events: firstEvents}, {events: secondEvents}, {events: make(chan ipc.Event)}},
				runs: []*ipc.RunInfo{
					{ID: "run-1", Status: types.RunRunning, CIReady: watch, Steps: []ipc.StepResultInfo{{StepName: types.StepCI, Status: types.StepStatusRunning}}},
					{ID: "run-1", Status: types.RunRunning, CIReady: true, Steps: []ipc.StepResultInfo{{StepName: types.StepCI, Status: types.StepStatusCompleted}}},
					{ID: "run-1", Status: types.RunChecksPassed, CIReady: true, Steps: []ipc.StepResultInfo{{StepName: types.StepCI, Status: types.StepStatusCompleted}}},
				},
			}
			reconciler := newRunReconciler(source, "run-1")
			defer reconciler.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			run, ready, err := driveRunWithReconciler(ctx, io.Discard, nil, reconciler, "run-1", false)
			if err != nil {
				t.Fatal(err)
			}
			if ready != watch {
				t.Fatalf("live-monitor stop = %v, want %v", ready, watch)
			}
			if !watch && run.Status != types.RunChecksPassed {
				t.Fatalf("release stopped before terminal completion: %s", run.Status)
			}
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := renderDriveResult(cmd, run, ready); err != nil {
				t.Fatal(err)
			}
			if watch {
				if !strings.Contains(out.String(), "the CI monitor rebases onto the base") || strings.Contains(out.String(), "nothing is left watching the PR") {
					t.Fatalf("wrong watch guidance: %s", &out)
				}
			} else if !strings.Contains(out.String(), "nothing is left watching the PR") || strings.Contains(out.String(), "the CI monitor rebases onto the base") {
				t.Fatalf("wrong release guidance: %s", &out)
			}
		})
	}
}

func TestOutcomeForRun_ChecksPassedOverrides(t *testing.T) {
	for _, rv := range []runView{
		{Status: string(types.RunChecksPassed), TestOverrideReason: "approved Test exception"},
		{Status: string(types.RunChecksPassed), CIOverrideReason: "approved CI exception"},
	} {
		if got := outcomeForRun(rv); got != "passed-with-override" {
			t.Fatalf("outcome = %s", got)
		}
	}
}

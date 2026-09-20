package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiTestExceptionOutput(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		status        types.RunStatus
		ciReady       bool
	}{
		{"completed", "passed-with-override", types.RunCompleted, false},
		{"checks-ready", "checks-passed", types.RunRunning, true},
		{"failed", "failed", types.RunFailed, false},
		{"cancelled", "cancelled", types.RunCancelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&out)
			const reason = "approved synthetic Test exception"
			run := &ipc.RunInfo{ID: "synthetic", Status: tc.status, TestOverrideReason: reason,
				Steps: []ipc.StepResultInfo{{StepName: types.StepCI, Status: types.StepStatusSkipped, SkipReason: "provider unavailable"}}}
			_ = renderDriveResult(cmd, run, tc.ciReady)
			var doc struct {
				Outcome string `toon:"outcome"`
				Run     struct {
					Reason string             `toon:"test_override_reason"`
					Skips  []automaticSkipRow `toon:"automatic_skips"`
				} `toon:"run"`
			}
			if err := toon.UnmarshalString(out.String(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Outcome != tc.outcome || doc.Run.Reason != reason || len(doc.Run.Skips) != 1 {
				t.Fatalf("exception or independent skip evidence lost: %+v", doc)
			}
		})
	}
}

func TestRunViewFromDBQualifiesLegacyTestOverrides(t *testing.T) {
	const condition = "configured test command failed with exit code 7"
	const reason = "operator explanation"
	for _, approval := range []*string{nil, strptr(reason)} {
		run := &db.Run{Status: types.RunCompleted}
		steps := []*db.StepResult{{StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: strptr(condition), ApprovalReason: approval}}
		view := runViewFromDB(run, steps, nil)
		if outcomeForRun(view) != "passed-with-override" || !strings.Contains(view.TestOverrideReason, condition) || view.CIOverrideReason != "" {
			t.Fatalf("DB fallback lost Test override: %+v", view)
		}
		if approval != nil && !strings.Contains(view.TestOverrideReason, reason) {
			t.Fatalf("operator reason lost: %+v", view)
		}
	}
}

func TestRunViewFromDBQualifiesOnlyExceptionEvidence(t *testing.T) {
	const reason = "operator explanation"
	for _, tc := range []struct {
		name, findings, outcome string
	}{
		{"no-go", `{"findings":[],"verdict":"no-go"}`, "passed-with-override"},
		{"inconclusive", `{"findings":[],"verdict":"inconclusive"}`, "passed-with-override"},
		{"no-surface", `{"findings":[],"verdict":"no-surface"}`, "passed"},
		{"go", `{"findings":[],"verdict":"go"}`, "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &db.Run{Status: types.RunCompleted}
			steps := []*db.StepResult{{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: strptr(tc.findings), ApprovalReason: strptr(reason)}}
			view := runViewFromDB(run, steps, nil)
			if got := outcomeForRun(view); got != tc.outcome {
				t.Fatalf("outcome = %q, want %q (%+v)", got, tc.outcome, view)
			}
			if tc.outcome == "passed-with-override" && !strings.Contains(view.TestOverrideReason, reason) {
				t.Fatalf("operator reason lost: %+v", view)
			}
			if steps[0].ApprovalReason == nil || *steps[0].ApprovalReason != reason {
				t.Fatalf("approval reason not retained: %+v", steps[0])
			}
		})
	}
}

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIntentSkipJourney(t *testing.T) {
	for _, tc := range []struct {
		name, config, cause string
	}{
		{"disabled", "intent:\n  enabled: false\n", "disabled in config"},
		{"no transcript", "intent:\n  enabled: true\n", "no matching agent transcript"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "codex", GlobalConfigExtra: tc.config})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "feature/missing-intent"
			h.CommitChange(branch, "feature.txt", "synthetic change\n", "add feature")
			h.PushToGate(branch)
			run := h.WaitForRun(branch, 90*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run = %s, error = %v", run.Status, run.Error)
			}
			step, ok := findStep(run.Steps, types.StepIntent)
			if !ok || step.Status != types.StepStatusSkipped || !strings.Contains(step.SkipReason, tc.cause) {
				t.Fatalf("intent skip lost its cause: %+v", step)
			}
			if cols := readRunIntent(t, h.NMHome, run.ID); cols.summary != nil {
				t.Fatalf("missing intent was replaced: %v", cols.summary)
			}
			// This is the actual CLI output from the completed hook-started run.
			out, err := h.Run("axi", "status")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			var doc struct {
				Outcome string `toon:"outcome"`
				Run     struct {
					AutomaticSkips []struct {
						Step   string `toon:"step"`
						Reason string `toon:"reason"`
					} `toon:"automatic_skips"`
				} `toon:"run"`
			}
			if err := toon.UnmarshalString(out, &doc); err != nil {
				t.Fatalf("parse status: %v\n%s", err, out)
			}
			found := false
			for _, skip := range doc.Run.AutomaticSkips {
				if skip.Step == "intent" && skip.Reason == step.SkipReason {
					found = true
				}
			}
			if doc.Outcome != "passed-with-skips" || !found {
				t.Fatalf("status omitted missing intent: %+v\n%s", doc, out)
			}
		})
	}
}

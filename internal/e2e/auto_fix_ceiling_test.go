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

// TestAutoFixCeilingJourney drives the real daemon with an operator
// auto_fix_ceiling and a pushed .no-mistakes.yaml asking for more, and reads
// back the limit each step recorded.
func TestAutoFixCeilingJourney(t *testing.T) {
	const ceilingLine = "operator auto_fix_ceiling lowered the "

	cases := []struct {
		name      string
		global    string // appended to the harness's global config
		overrides string // repository_overrides entry body for machineLocalRemote, if any
		repo      string // pushed .no-mistakes.yaml
		want      map[types.StepName]int
		capped    []types.StepName
	}{
		{
			name:   "global ceiling 0 beats a pushed 3",
			global: "auto_fix_ceiling:\n  test: 0\n  rebase: 0\n",
			repo:   "auto_fix:\n  test: 3\n  rebase: 3\n",
			want:   map[types.StepName]int{types.StepTest: 0, types.StepRebase: 0},
			capped: []types.StepName{types.StepTest, types.StepRebase},
		},
		{
			name:   "pushed value below the ceiling is kept",
			global: "auto_fix_ceiling:\n  test: 2\n",
			repo:   "auto_fix:\n  test: 1\n",
			want:   map[types.StepName]int{types.StepTest: 1},
		},
		{
			name: "no ceiling keeps the pushed value",
			repo: "auto_fix:\n  test: 3\n",
			want: map[types.StepName]int{types.StepTest: 3},
		},
		{
			name:      "repository override ceiling applies to its remote",
			overrides: "    auto_fix_ceiling:\n      test: 1\n",
			repo:      "auto_fix:\n  test: 3\n",
			want:      map[types.StepName]int{types.StepTest: 1},
			capped:    []types.StepName{types.StepTest},
		},
		{
			name:   "repository override for another remote does not apply",
			global: "repository_overrides:\n  https://example.invalid/acme/other.git:\n    auto_fix_ceiling:\n      test: 1\n",
			repo:   "auto_fix:\n  test: 3\n",
			want:   map[types.StepName]int{types.StepTest: 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			if tc.global != "" {
				globalConfig := filepath.Join(h.NMHome, "config.yaml")
				data, err := os.ReadFile(globalConfig)
				if err != nil {
					t.Fatalf("read global config: %v", err)
				}
				if err := os.WriteFile(globalConfig, append(data, tc.global...), 0o644); err != nil {
					t.Fatalf("write global config: %v", err)
				}
			}
			// Always points origin at machineLocalRemote, so an override keyed to
			// another remote is a real non-match.
			setupMachineLocalCommands(t, h, tc.overrides)

			branch := "feature/ceiling-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
			h.CommitChange(branch, ".no-mistakes.yaml", tc.repo, "ask for auto-fix budget")
			h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
			h.PushToGate(branch)
			run := h.WaitForRun(branch, 120*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
			}

			for step, want := range tc.want {
				got, ok := findStep(run.Steps, step)
				if !ok {
					t.Fatalf("run has no %s step", step)
				}
				if got.AutoFixLimit != want {
					t.Errorf("%s recorded auto_fix_limit = %d, want %d", step, got.AutoFixLimit, want)
				}
				capped := false
				for _, c := range tc.capped {
					capped = capped || c == step
				}
				if log := readStepLog(t, h, run.ID, string(step)); strings.Contains(log, ceilingLine) != capped {
					t.Errorf("%s log ceiling line present = %v, want %v\n%s", step, !capped, capped, log)
				}
			}
		})
	}
}

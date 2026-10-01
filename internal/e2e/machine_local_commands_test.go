//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	machineLocalRemote     = "https://example.invalid/acme/widget.git"
	testStepPromptMarker   = "You are validating a code change by driving the product itself"
	machineLocalConfigFile = "command-config.ndjson"
)

// setupMachineLocalCommands writes the operator's global repository override,
// initializes the gate, and points origin at a forge-shaped URL that matches it.
func setupMachineLocalCommands(t *testing.T, h *Harness, overrides string) {
	t.Helper()
	if overrides != "" {
		globalConfig := filepath.Join(h.NMHome, "config.yaml")
		data, err := os.ReadFile(globalConfig)
		if err != nil {
			t.Fatalf("read global config: %v", err)
		}
		source := string(data) + "repository_overrides:\n  " + machineLocalRemote + ":\n" + overrides
		if err := os.WriteFile(globalConfig, []byte(source), 0o644); err != nil {
			t.Fatalf("write global config: %v", err)
		}
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, machineLocalRemote, h.UpstreamDir)
	if out, err := h.runGit(context.Background(), h.WorkDir, "remote", "set-url", "origin", machineLocalRemote); err != nil {
		t.Fatalf("set forge-shaped origin: %v\n%s", err, out)
	}
}

func writeMachineLocalScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func testStepPrompts(h *Harness) []string {
	var prompts []string
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, testStepPromptMarker) {
			prompts = append(prompts, inv.Prompt)
		}
	}
	return prompts
}

// TestMachineLocalCommandOverridesJourney drives the real daemon with an
// operator-only repository override that adds a local test check and lowers
// the team command's priority.
func TestMachineLocalCommandOverridesJourney(t *testing.T) {
	t.Run("overrides_apply_are_declared_and_recorded", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		marker := filepath.Join(t.TempDir(), "marker")
		writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-team-test"),
			`printf 'team nice=%s\n' "$(ps -o nice= -p $$)" >> "`+marker+`"
exit 0
`)
		writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-local-smoke"),
			`printf 'local nice=%s\n' "$(ps -o nice= -p $$)" >> "`+marker+`"
exit 0
`)
		setupMachineLocalCommands(t, h, `    commands:
      test:
        additional:
          - nm-local-smoke
        nice: 7
`)
		const branch = "feature/local-overrides"
		h.CommitChange(branch, ".no-mistakes.yaml", "commands:\n  test: nm-team-test\n", "configure team test command")
		h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
		h.PushToGate(branch)
		run := h.WaitForRun(branch, 120*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
		}

		markerData, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("read marker: %v", err)
		}
		t.Logf("command marker:\n%s", markerData)
		for _, want := range []string{
			"team nice=7",
			"local nice=7",
		} {
			if !strings.Contains(string(markerData), want) {
				t.Errorf("marker missing %q", want)
			}
		}

		testLog := readStepLog(t, h, run.ID, string(types.StepTest))
		t.Logf("test step log:\n%s", testLog)
		for _, want := range []string{
			`machine-local overrides applied to commands.test: nice 7; additional checks "nm-local-smoke"`,
			"running tests: nm-team-test",
			"running machine-local test check: nm-local-smoke",
		} {
			if !strings.Contains(testLog, want) {
				t.Errorf("test step log missing %q", want)
			}
		}

		prompts := testStepPrompts(h)
		if len(prompts) == 0 {
			t.Fatal("test step never invoked the agent")
		}
		for _, prompt := range prompts {
			for _, want := range []string{
				"Baseline ran with machine-local overrides applied to commands.test: nice 7; additional checks \"nm-local-smoke\"",
				"Configured test command already ran successfully as baseline: `nm-team-test`",
				"Machine-local test check (the operator's addition, not the repository's command) already ran successfully as baseline: `nm-local-smoke`",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("test prompt missing %q", want)
				}
			}
		}

		configPath := filepath.Join(h.NMHome, "logs", run.ID, machineLocalConfigFile)
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("stat command config evidence: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("command config evidence mode = %v, want 0600", info.Mode().Perm())
		}
		file, err := os.Open(configPath)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(nil, 4<<20)
		records := 0
		for scanner.Scan() {
			var record struct {
				Event           string `json:"event"`
				EffectiveConfig struct {
					Commands         map[string]any `json:"Commands"`
					CommandOverrides map[string]struct {
						Nice       int      `json:"nice"`
						Additional []string `json:"additional"`
					} `json:"CommandOverrides"`
				} `json:"effective_config"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				t.Fatalf("parse command config record: %v", err)
			}
			records++
			override := record.EffectiveConfig.CommandOverrides["test"]
			t.Logf("command-config record event=%s commands=%v test override=%+v", record.Event, record.EffectiveConfig.Commands, override)
			if record.Event != "start" || override.Nice != 7 ||
				len(override.Additional) != 1 || override.Additional[0] != "nm-local-smoke" {
				t.Errorf("unexpected command config record: %s", scanner.Text())
			}
		}
		if records != 1 {
			t.Fatalf("command config records = %d, want 1", records)
		}
	})

	t.Run("failing_additional_check_is_attributed_by_name_even_without_team_command", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-local-fail"), "echo local smoke broke\nexit 3\n")
		setupMachineLocalCommands(t, h, `    commands:
      test:
        additional:
          - nm-local-fail
`)
		const branch = "feature/local-failure"
		h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
		h.PushToGate(branch)
		run := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 120*time.Second)
		step, ok := findStep(run.Steps, types.StepTest)
		if !ok || step.FindingsJSON == nil {
			t.Fatal("test findings missing")
		}
		t.Logf("test findings: %s", *step.FindingsJSON)
		findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, finding := range findings.Items {
			if strings.Contains(finding.Description, "configured test command failed") {
				t.Errorf("local failure attributed to the team command: %q", finding.Description)
			}
			if finding.Description == "machine-local test check failed with exit code 3: nm-local-fail" {
				found = true
			}
		}
		if !found {
			t.Errorf("no finding names the failing machine-local check")
		}
		prompts := testStepPrompts(h)
		if len(prompts) == 0 {
			t.Fatal("test step never invoked the agent")
		}
		if !strings.Contains(prompts[0], "Machine-local test check (the operator's addition, not the repository's command) failed with exit code 3: `nm-local-fail`") {
			t.Errorf("test prompt does not name the failing machine-local check")
		}
		testLog := readStepLog(t, h, run.ID, string(types.StepTest))
		t.Logf("test step log:\n%s", testLog)
		if !strings.Contains(testLog, "machine-local test check failed: nm-local-fail") || strings.Contains(testLog, "no test command configured") {
			t.Errorf("test log misattributes the local failure")
		}
		h.CancelRun(run.ID)
	})

	t.Run("failing_additional_lint_check_parks_lint", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-local-policy"), "echo policy violated\nexit 4\n")
		setupMachineLocalCommands(t, h, `    commands:
      lint:
        additional:
          - nm-local-policy
`)
		const branch = "feature/local-lint"
		h.CommitChange(branch, ".no-mistakes.yaml", "commands:\n  lint: 'true'\n", "configure team lint")
		h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
		h.PushToGate(branch)
		run := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 120*time.Second)
		step, _ := findStep(run.Steps, types.StepLint)
		if step.FindingsJSON == nil {
			t.Fatal("lint findings missing")
		}
		t.Logf("lint findings: %s", *step.FindingsJSON)
		if !strings.Contains(*step.FindingsJSON, "machine-local lint check failed with exit code 4: nm-local-policy") {
			t.Errorf("lint finding does not name the machine-local check")
		}
		h.CancelRun(run.ID)
	})

	t.Run("repository_config_cannot_declare_machine_local_checks", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		marker := filepath.Join(t.TempDir(), "marker")
		writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-pushed-check"), `echo ran >> "`+marker+`"
exit 0
`)
		setupMachineLocalCommands(t, h, "")
		const branch = "feature/pushed-overrides"
		h.CommitChange(branch, ".no-mistakes.yaml", "repository_overrides:\n  "+machineLocalRemote+":\n    commands:\n      test:\n        additional:\n          - nm-pushed-check\n", "try to declare a machine-local check from the repository")
		h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
		h.PushToGate(branch)
		run := h.WaitForRun(branch, 120*time.Second)
		t.Logf("run status = %s (error=%v)", run.Status, run.Error)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("a repository-declared machine-local check executed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(h.NMHome, "logs", run.ID, machineLocalConfigFile)); !os.IsNotExist(err) {
			t.Errorf("command config evidence written for a repository-declared override: %v", err)
		}
	})

	t.Run("no_override_behaves_as_today", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-team-test"), "exit 0\n")
		setupMachineLocalCommands(t, h, "")
		const branch = "feature/no-override"
		h.CommitChange(branch, ".no-mistakes.yaml", "commands:\n  test: nm-team-test\n", "configure team test command")
		h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
		h.PushToGate(branch)
		run := h.WaitForRun(branch, 120*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run status = %s (error=%v)", run.Status, run.Error)
		}
		if _, err := os.Stat(filepath.Join(h.NMHome, "logs", run.ID, machineLocalConfigFile)); !os.IsNotExist(err) {
			t.Errorf("command config evidence written without an override: %v", err)
		}
		testLog := readStepLog(t, h, run.ID, string(types.StepTest))
		if strings.Contains(testLog, "machine-local") {
			t.Errorf("test log mentions machine-local overrides without any configured")
		}
		for _, prompt := range testStepPrompts(h) {
			if strings.Contains(prompt, "machine-local") || strings.Contains(prompt, "Machine-local") {
				t.Errorf("test prompt mentions machine-local overrides without any configured")
			}
		}
	})
}

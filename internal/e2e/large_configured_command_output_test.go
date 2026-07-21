//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const largeCommandMiddleMarker = "OMITTED_SECRET_MIDDLE_MARKER_7f9c"

func TestLargeConfiguredTestAndLintFailuresRemainUsableThroughAXIFix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		step       types.StepName
		branch     string
		commandKey string
		findingID  string
	}{
		{name: "test", step: types.StepTest, branch: "large-test-output", commandKey: "test", findingID: "test-1"},
		{name: "lint", step: types.StepLint, branch: "large-lint-output", commandKey: "lint", findingID: "lint-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}

			commandName := "nm-large-" + tc.name + "-failure"
			commandPath := filepath.Join(h.BinDir, commandName)
			script := `#!/bin/sh
printf 'HEAD_MARKER context line\n'
head -c 1048576 /dev/zero | tr '\000' A
printf '` + largeCommandMiddleMarker + `'
head -c 1048576 /dev/zero | tr '\000' B
printf '\nTAIL_MARKER 最后的错误🙂\n'
exit 1
`
			if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
				t.Fatalf("write large failure command: %v", err)
			}

			config := "allow_repo_commands: true\ncommands:\n  test: true\n  lint: true\n"
			config = strings.Replace(config, "  "+tc.commandKey+": true", "  "+tc.commandKey+": "+commandName, 1)
			h.CommitChange(tc.branch, ".no-mistakes.yaml", config, "configure large "+tc.name+" failure")
			h.PushToGate(tc.branch)
			run := waitForStepStatus(t, h, tc.branch, tc.step, types.StepStatusAwaitingApproval, 60*time.Second)

			step, ok := findStep(run.Steps, tc.step)
			if !ok || step.FindingsJSON == nil {
				t.Fatalf("%s findings missing", tc.step)
			}
			findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
			if err != nil {
				t.Fatalf("parse %s findings: %v", tc.step, err)
			}
			wantOriginalBytes := len("HEAD_MARKER context line\n") + 2*1048576 + len(largeCommandMiddleMarker) + len("\nTAIL_MARKER 最后的错误🙂\n")
			for _, want := range []string{
				"HEAD_MARKER context line",
				"TAIL_MARKER 最后的错误🙂",
				fmt.Sprintf("original byte count: %d", wantOriginalBytes),
				"complete output: " + strings.ToUpper(tc.name[:1]) + tc.name[1:] + " step log",
			} {
				if !strings.Contains(findings.Summary, want) {
					t.Errorf("%s bounded summary missing %q", tc.step, want)
				}
			}
			if strings.Contains(findings.Summary, largeCommandMiddleMarker) {
				t.Fatalf("%s findings retained omitted middle output", tc.step)
			}
			if len(*step.FindingsJSON) >= 128*1024 {
				t.Fatalf("%s findings payload is still oversized: %d bytes", tc.step, len(*step.FindingsJSON))
			}

			status, err := h.Run("axi", "status")
			if err != nil {
				t.Fatalf("axi status failed for bounded %s findings: %v\n%s", tc.step, err, status)
			}
			if strings.Contains(status, "bufio.Scanner: token too long") {
				t.Fatalf("AXI scanner still overflowed:\n%s", status)
			}

			response, err := h.Run("axi", "respond", "--action", "fix", "--findings", tc.findingID)
			if err != nil {
				t.Fatalf("AXI fix response for large %s output failed: %v\n%s", tc.step, err, response)
			}
			if strings.Contains(response, "argument list too long") || strings.Contains(response, "bufio.Scanner: token too long") {
				t.Fatalf("large %s fix hit an old size limit:\n%s", tc.step, response)
			}
			fixed := waitForStepStatus(t, h, tc.branch, tc.step, types.StepStatusFixReview, 60*time.Second)
			if fixed.Status != types.RunRunning {
				t.Fatalf("run status after %s fix = %s", tc.step, fixed.Status)
			}

			logData := readStepLog(t, h, run.ID, string(tc.step))
			for _, want := range []string{"HEAD_MARKER context line", largeCommandMiddleMarker, "TAIL_MARKER 最后的错误🙂"} {
				if !strings.Contains(logData, want) {
					t.Errorf("full %s log missing %q", tc.step, want)
				}
			}
			if len(logData) < 2*1024*1024 {
				t.Fatalf("full %s log was truncated to %d bytes", tc.step, len(logData))
			}

			var repairPrompt string
			for _, invocation := range h.AgentInvocations() {
				if strings.Contains(invocation.Prompt, "Previous "+tc.name+" findings to address") {
					repairPrompt = invocation.Prompt
				}
			}
			if repairPrompt == "" {
				t.Fatalf("no %s repair prompt recorded", tc.step)
			}
			for _, want := range []string{"HEAD_MARKER context line", "TAIL_MARKER 最后的错误🙂", fmt.Sprintf("--step %s --full", tc.name)} {
				if !strings.Contains(repairPrompt, want) {
					t.Errorf("%s repair prompt missing %q", tc.step, want)
				}
			}
			if strings.Contains(repairPrompt, largeCommandMiddleMarker) {
				t.Fatalf("%s repair prompt retained omitted middle output", tc.step)
			}
		})
	}
}

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const structuredGateScript = `#!/bin/sh
set -eu

index="$1"
mode=$(cat gate-mode.txt)
report="$NO_MISTAKES_FINDINGS_FILE"

case "$report" in
  /*) ;;
  *) echo "findings path is not absolute: $report"; exit 91 ;;
esac
case "$report" in
  "$PWD"/*) echo "findings path is inside the worktree: $report"; exit 92 ;;
esac
if [ ! -f "$report" ] || [ -s "$report" ]; then
  echo "findings file did not start as an empty regular file: $report"
  exit 93
fi

echo "findings-path=$report"
echo "gate-mode=$mode gate-index=$index"

case "$mode:$index" in
  legacy:*)
    printf '%s' "$report" >"$NM_HOME/legacy-gate-path-$index.txt"
    ;;
  warning:0)
    printf '%s' '{"findings":[{"id":"score-warning","severity":"warning","file":"score.go","line":7,"description":"score is near the budget"}]}' >"$report"
    ;;
  error-zero:0)
    printf '%s' '{"findings":[{"id":"score-error","severity":"error","file":"score.go","line":9,"description":"score is below the budget","action":"auto-fix"}]}' >"$report"
    ;;
  nonzero-valid:0)
    printf '%s' '{"findings":[{"id":"exit-warning","severity":"warning","description":"command also exited seven","action":"no-op"}]}' >"$report"
    echo "valid report before exit seven"
    exit 7
    ;;
  nonzero-empty:0)
    echo "empty report before exit seven"
    exit 7
    ;;
  reserved:0)
    printf '%s' '{"findings":[{"id":"test-agent-timeout","severity":"error","description":"must not acquire pipeline semantics"}]}' >"$report"
    ;;
  malformed:0)
    printf '%s' '{' >"$report"
    ;;
  unknown-field:0)
    printf '%s' '{"findings":[{"id":"unknown","severity":"error","description":"strict schema","source":"user"}]}' >"$report"
    ;;
  over-cap:0)
    head -c 1048577 /dev/zero | tr '\000' ' ' >"$report"
    echo "over-cap-report-bytes=$(wc -c <"$report" | tr -d ' ')"
    ;;
  over-count:0)
    printf '%s' '{"findings":[' >"$report"
    item=0
    while [ "$item" -lt 501 ]; do
      if [ "$item" -gt 0 ]; then printf '%s' ',' >>"$report"; fi
      printf '{"id":"finding-%s","severity":"info","description":"over count"}' "$item" >>"$report"
      item=$((item + 1))
    done
    printf '%s' ']}' >>"$report"
    echo "over-count-report-items=$item"
    ;;
  transport:0)
    printf '%s' '{"findings":[{"id":"long","severity":"error","description":"' >"$report"
    head -c 200000 /dev/zero | tr '\000' '<' >>"$report"
    printf '%s' '"}]}' >>"$report"
    echo "transport-report-bytes=$(wc -c <"$report" | tr -d ' ')"
    ;;
  aggregate:0)
    printf '%s' '{"findings":[{"id":"aggregate-large","severity":"info","description":"' >"$report"
    head -c 450000 /dev/zero | tr '\000' x >>"$report"
    printf '%s' '","action":"no-op"}]}' >>"$report"
    echo "aggregate-report-0-bytes=$(wc -c <"$report" | tr -d ' ')"
    ;;
  aggregate:*)
    printf '%s' "{\"findings\":[{\"id\":\"aggregate-$index\",\"severity\":\"info\",\"description\":\"" >"$report"
    head -c 20000 /dev/zero | tr '\000' x >>"$report"
    printf '%s' '","action":"no-op"}]}' >>"$report"
    echo "aggregate-report-$index-bytes=$(wc -c <"$report" | tr -d ' ')"
    ;;
esac
`

func structuredGatesConfig() string {
	var b strings.Builder
	b.WriteString("ignore_patterns:\n  - 'vendor/**'\nallow_repo_commands: false\ngates:\n")
	for i := 0; i < 9; i++ {
		fmt.Fprintf(&b, "  - name: report-%d\n    after: rebase\n    command: sh scripts/structured-gate.sh %d\n", i, i)
	}
	return b.String()
}

func structuredGateScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "structured-gate-scenario.yaml")
	content := `actions:
  - match: 'repository gate "report-0"'
    text: "cleared the reported gate error"
    edits:
      - path: "gate-mode.txt"
        old: "error-zero\n"
        new: "legacy\n"
    structured:
      summary: "clear reported gate error"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write structured gate scenario: %v", err)
	}
	return path
}

func TestCustomGateStructuredFindingsJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: structuredGateScenario(t)})
	h.CommitChange("main", "scripts/structured-gate.sh", structuredGateScript, "maintainer: add structured gate check")
	pushMainRepoConfig(t, h, structuredGatesConfig())
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	t.Run("empty_file_keeps_legacy_pass_and_paths_are_private", func(t *testing.T) {
		const branch = "feature/gate-findings-legacy"
		h.CommitChange(branch, "gate-mode.txt", "legacy\n", "select legacy gate mode")
		h.PushToGate(branch)
		fw := h.AddWorktree(branch)
		run := h.WaitForRun(branch, 180*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run status = %s, want completed (error=%q)", run.Status, deref(run.Error))
		}

		seen := map[string]bool{}
		for i := 0; i < 9; i++ {
			step := gateReportStep(i)
			pathBytes, err := os.ReadFile(filepath.Join(h.NMHome, fmt.Sprintf("legacy-gate-path-%d.txt", i)))
			if err != nil {
				t.Fatalf("read path recorded by %s: %v", step, err)
			}
			path := strings.TrimSpace(string(pathBytes))
			log, err := h.RunInDir(fw, "axi", "logs", "--step", string(step), "--full")
			if err != nil {
				t.Fatalf("axi logs --step %s: %v\n%s", step, err, log)
			}
			if !filepath.IsAbs(path) || strings.HasPrefix(path, fw+string(os.PathSeparator)) {
				t.Fatalf("gate path %q is not absolute and external to %q", path, fw)
			}
			if seen[path] {
				t.Fatalf("gate reused findings path %q", path)
			}
			seen[path] = true
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("gate findings path %q still exists after command: %v", path, err)
			}
			if i == 0 {
				t.Logf("EVIDENCE legacy gate received private path %s, which no longer exists; completed step log:\n%s", path, log)
			}
		}
	})

	for _, tc := range []struct {
		mode            string
		wantID          string
		wantSeverity    string
		wantAction      string
		wantDescription string
		wantLog         string
	}{
		{mode: "warning", wantID: "score-warning", wantSeverity: types.FindingSeverityWarning, wantAction: types.ActionAskUser, wantDescription: "score is near the budget", wantLog: "gate-mode=warning gate-index=0"},
		{mode: "error-zero", wantID: "score-error", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAutoFix, wantDescription: "score is below the budget", wantLog: "gate-mode=error-zero gate-index=0"},
		{mode: "nonzero-valid", wantID: "exit-warning", wantSeverity: types.FindingSeverityWarning, wantAction: types.ActionNoOp, wantDescription: "command also exited seven", wantLog: "valid report before exit seven"},
		{mode: "nonzero-empty", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAskUser, wantDescription: "failed with exit code 7", wantLog: "empty report before exit seven"},
		{mode: "reserved", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAskUser, wantDescription: "uses a reserved id", wantLog: "gate-mode=reserved gate-index=0"},
		{mode: "malformed", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAskUser, wantDescription: "parse findings JSON", wantLog: "gate-mode=malformed gate-index=0"},
		{mode: "unknown-field", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAskUser, wantDescription: "unknown field", wantLog: "gate-mode=unknown-field gate-index=0"},
		{mode: "over-cap", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAskUser, wantDescription: "exceeds 1 MiB", wantLog: "over-cap-report-bytes=1048577"},
		{mode: "over-count", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAskUser, wantDescription: "exceeds 500 findings", wantLog: "over-count-report-items=501"},
		{mode: "transport", wantSeverity: types.FindingSeverityError, wantAction: types.ActionAskUser, wantDescription: "too large to transport", wantLog: "transport-report-bytes=200064"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			branch := "feature/gate-findings-" + tc.mode
			h.CommitChange(branch, "gate-mode.txt", tc.mode+"\n", "select "+tc.mode+" gate mode")
			h.PushToGate(branch)
			fw := h.AddWorktree(branch)
			parked := waitForStepStatus(t, h, branch, gateReportStep(0), types.StepStatusAwaitingApproval, 180*time.Second)
			finding := onlyGateFinding(t, parked, gateReportStep(0))
			if tc.wantID != "" && finding.ID != tc.wantID {
				t.Errorf("finding id = %q, want %q", finding.ID, tc.wantID)
			}
			if finding.Severity != tc.wantSeverity || finding.Action != tc.wantAction || !strings.Contains(finding.Description, tc.wantDescription) {
				t.Fatalf("finding = %+v, want severity %q action %q description containing %q", finding, tc.wantSeverity, tc.wantAction, tc.wantDescription)
			}

			status, err := h.RunInDir(fw, "axi", "status")
			if err != nil {
				t.Fatalf("axi status: %v\n%s", err, status)
			}
			for _, want := range []string{string(gateReportStep(0)), tc.wantDescription} {
				if !strings.Contains(status, want) {
					t.Errorf("axi status is missing %q\n%s", want, status)
				}
			}
			log, err := h.RunInDir(fw, "axi", "logs", "--step", string(gateReportStep(0)), "--full")
			if err != nil {
				t.Fatalf("axi logs: %v\n%s", err, log)
			}
			for _, want := range []string{tc.wantDescription, tc.wantLog} {
				if !strings.Contains(log, want) {
					t.Errorf("axi logs is missing %q\n%s", want, log)
				}
			}
			if tc.mode == "warning" && runtime.GOOS == "linux" {
				tuiOutput := captureGateTUI(t, h, fw, tc.wantDescription)
				plain := ansi.Strip(tuiOutput)
				if !strings.Contains(plain, tc.wantDescription) {
					t.Fatalf("live TUI is missing %q, captured bytes=%d", tc.wantDescription, len(tuiOutput))
				}
				t.Logf("EVIDENCE live TUI excerpt from a 40x140 PTY:\n%s", textAround(plain, tc.wantDescription, 500))
			}
			t.Logf("EVIDENCE %s axi status:\n%s\nEVIDENCE %s gate log:\n%s", tc.mode, status, tc.mode, log)

			if tc.mode == "error-zero" {
				fixOutput, err := h.RunInDir(fw, "axi", "respond", "--action", "fix", "--findings", tc.wantID)
				if err != nil {
					t.Fatalf("axi respond --action fix --findings %s: %v\n%s", tc.wantID, err, fixOutput)
				}
				t.Logf("EVIDENCE stable gate finding selected for fix:\n%s", fixOutput)
			} else {
				h.Respond(parked.ID, gateReportStep(0), types.ActionApprove)
			}
			completed := h.WaitForRun(branch, 180*time.Second)
			if completed.Status != types.RunCompleted {
				t.Fatalf("run status after approval = %s, want completed (error=%q)", completed.Status, deref(completed.Error))
			}
		})
	}

	t.Run("aggregate_budget_keeps_status_readable", func(t *testing.T) {
		const branch = "feature/gate-findings-aggregate"
		h.CommitChange(branch, "gate-mode.txt", "aggregate\n", "select aggregate gate mode")
		h.PushToGate(branch)
		fw := h.AddWorktree(branch)

		var runID string
		var lastParkedStatus string
		for i := 1; i < 9; i++ {
			step := gateReportStep(i)
			parked := waitForStepStatus(t, h, branch, step, types.StepStatusAwaitingApproval, 180*time.Second)
			runID = parked.ID
			finding := onlyGateFinding(t, parked, step)
			if finding.Severity != types.FindingSeverityError || finding.Action != types.ActionAskUser || !strings.Contains(finding.Description, "too large to transport") {
				t.Fatalf("gate %s refusal = %+v", step, finding)
			}
			status, err := h.RunInDir(fw, "axi", "status", "--run", runID)
			if err != nil {
				t.Fatalf("aggregate axi status at %s: %v\n%s", step, err, status)
			}
			if !strings.Contains(status, string(step)) || !strings.Contains(status, "too large to transport") {
				t.Fatalf("aggregate axi status at %s lost its current refusal\n%s", step, status)
			}
			lastParkedStatus = status
			h.Respond(parked.ID, step, types.ActionApprove)
		}
		completed := h.WaitForRun(branch, 180*time.Second)
		if completed.Status != types.RunCompleted {
			t.Fatalf("aggregate run status = %s, want completed (error=%q)", completed.Status, deref(completed.Error))
		}

		first, ok := findStep(completed.Steps, gateReportStep(0))
		if !ok || first.FindingsJSON == nil {
			t.Fatalf("aggregate first gate findings are missing")
		}
		parsed, err := types.ParseFindingsJSON(*first.FindingsJSON)
		if err != nil || len(parsed.Items) != 1 || parsed.Items[0].ID != "aggregate-large" || len(parsed.Items[0].Description) != 450000 {
			t.Fatalf("aggregate first gate findings = %+v, err=%v", parsed.Items, err)
		}

		status, err := h.RunInDir(fw, "axi", "status", "--run", runID)
		if err != nil {
			t.Fatalf("aggregate axi status: %v\n%s", err, status)
		}
		if !strings.Contains(status, runID) || !strings.Contains(status, "status: completed") {
			t.Fatalf("aggregate completed axi status lost the run outcome\n%s", status)
		}
		firstLog, err := h.RunInDir(fw, "axi", "logs", "--run", runID, "--step", string(gateReportStep(0)), "--full")
		if err != nil {
			t.Fatalf("aggregate first gate axi logs: %v\n%s", err, firstLog)
		}
		if !strings.Contains(firstLog, "aggregate-large") || len(firstLog) < 450000 {
			t.Fatalf("aggregate first gate log lost the near-full finding, bytes=%d", len(firstLog))
		}
		lastLog, err := h.RunInDir(fw, "axi", "logs", "--run", runID, "--step", string(gateReportStep(8)), "--full")
		if err != nil {
			t.Fatalf("aggregate axi logs: %v\n%s", err, lastLog)
		}
		for _, want := range []string{"too large to transport", "aggregate-report-8-bytes="} {
			if !strings.Contains(lastLog, want) {
				t.Errorf("aggregate last gate log is missing %q\n%s", want, lastLog)
			}
		}
		t.Logf("EVIDENCE aggregate eighth parked status:\n%s", lastParkedStatus)
		t.Logf("EVIDENCE aggregate completed status bytes=%d, first gate log bytes=%d, first description bytes=%d", len(status), len(firstLog), len(parsed.Items[0].Description))
		t.Logf("EVIDENCE aggregate last gate log:\n%s", lastLog)
	})
}

func gateReportStep(index int) types.StepName {
	return types.StepName("gate.rebase.report-" + strconv.Itoa(index))
}

func onlyGateFinding(t *testing.T, run *ipc.RunInfo, stepName types.StepName) types.Finding {
	t.Helper()
	step, ok := findStep(run.Steps, stepName)
	if !ok || step.FindingsJSON == nil {
		t.Fatalf("%s has no findings", stepName)
	}
	parsed, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse %s findings: %v", stepName, err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("%s findings count = %d, want 1", stepName, len(parsed.Items))
	}
	return parsed.Items[0]
}

func captureGateTUI(t *testing.T, h *Harness, worktree, marker string) string {
	t.Helper()
	scriptBin, err := exec.LookPath("script")
	if err != nil {
		t.Fatalf("live TUI requires script(1): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	inputR, inputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputW.Close()
	outputR, outputW, err := os.Pipe()
	if err != nil {
		inputR.Close()
		t.Fatal(err)
	}
	defer outputR.Close()

	command := "stty rows 40 cols 140; exec " + shellQuote(h.NMBin)
	cmd := exec.CommandContext(ctx, scriptBin, "-qfec", command, "/dev/null")
	cmd.Dir = worktree
	cmd.Env = mergedEnv(os.Environ(), map[string]string{"TERM": "xterm-256color"})
	cmd.Stdin = inputR
	cmd.Stdout = outputW
	cmd.Stderr = outputW
	if err := cmd.Start(); err != nil {
		inputR.Close()
		outputW.Close()
		t.Fatalf("start live TUI: %v", err)
	}
	inputR.Close()
	outputW.Close()

	outputCh := make(chan string, 1)
	go func() {
		defer outputR.Close()
		var captured strings.Builder
		buf := make([]byte, 8192)
		sentQuit := false
		for {
			n, readErr := outputR.Read(buf)
			if n > 0 {
				captured.Write(buf[:n])
				if !sentQuit && strings.Contains(ansi.Strip(captured.String()), marker) {
					_, _ = io.WriteString(inputW, "q")
					_ = inputW.Close()
					sentQuit = true
				}
			}
			if readErr != nil {
				break
			}
		}
		outputCh <- captured.String()
	}()

	waitErr := cmd.Wait()
	_ = inputW.Close()
	output := <-outputCh
	if ctx.Err() != nil {
		t.Fatalf("live TUI did not render %q before timeout, captured bytes=%d", marker, len(output))
	}
	if waitErr != nil {
		t.Fatalf("live TUI exited with error: %v\n%s", waitErr, ansi.Strip(output))
	}
	return output
}

func textAround(text, marker string, radius int) string {
	index := strings.Index(text, marker)
	if index < 0 {
		return text
	}
	start := index - radius
	if start < 0 {
		start = 0
	}
	end := index + len(marker) + radius
	if end > len(text) {
		end = len(text)
	}
	return text[start:end]
}

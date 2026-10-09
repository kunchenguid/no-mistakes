package steps

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func gateTestCommand(t *testing.T, dir, unixCommand, windowsCommand string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "gate-command.cmd")
		if err := os.WriteFile(path, []byte("@echo off\r\n"+windowsCommand+"\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "call gate-command.cmd"
	}
	return unixCommand
}

// The command writes the same protocol bytes a trusted repository check would.
func gateReportCommand(t *testing.T, dir string, exitCode int) string {
	t.Helper()
	return gateTestCommand(
		t,
		dir,
		fmt.Sprintf(`printf '%%s' "$NO_MISTAKES_FINDINGS_FILE" > gate-path.txt; cat gate-report.json > "$NO_MISTAKES_FINDINGS_FILE"; echo 'gate output'; exit %d`, exitCode),
		fmt.Sprintf("echo %%NO_MISTAKES_FINDINGS_FILE%%>gate-path.txt\r\ncopy /y gate-report.json \"%%NO_MISTAKES_FINDINGS_FILE%%\" >nul\r\necho gate output\r\nexit /b %d", exitCode),
	)
}

func gateFindingsReport(count int) string {
	items := make([]Finding, count)
	for i := range items {
		items[i] = Finding{ID: fmt.Sprintf("finding-%d", i), Severity: "info", Description: "budget checked", Action: types.ActionNoOp}
	}
	raw, err := json.Marshal(struct {
		Items []Finding `json:"findings"`
	}{Items: items})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestCustomGateStep_StructuredFindings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		report   string
		exitCode int
		park     bool
		count    int
	}{
		{"error on zero exit", `{"findings":[{"id":"budget-low","severity":"error","file":"score.go","line":7,"description":"score below budget","action":"auto-fix"}]}`, 0, true, 1},
		{"error marked no-op", `{"findings":[{"id":"budget-low","severity":"error","description":"score below budget","action":"no-op"}]}`, 0, true, 1},
		{"mixed severities", `{"findings":[{"id":"budget-low","severity":"info","description":"score measured","action":"no-op"},{"id":"budget-error","severity":"error","description":"score below budget"}]}`, 0, true, 2},
		{"warning on zero exit", `{"findings":[{"id":"budget-low","severity":"warning","description":"score near budget"}]}`, 0, false, 1},
		{"info on zero exit", `{"findings":[{"id":"budget-low","severity":"info","description":"score near budget","action":"no-op"}]}`, 0, false, 1},
		{"report on nonzero exit", `{"findings":[{"id":"budget-low","severity":"warning","description":"score near budget"}]}`, 7, true, 1},
		{"empty array on zero exit", `{"findings":[]}`, 0, false, 0},
		{"empty array on nonzero exit", `{"findings":[]}`, 7, true, 0},
		{"500 findings", gateFindingsReport(500), 0, false, 500},
		{"exactly 1 MiB", `{"findings":[]}` + strings.Repeat(" ", (1<<20)-len(`{"findings":[]}`)), 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			if err := os.WriteFile(filepath.Join(dir, "gate-report.json"), []byte(tc.report), 0o600); err != nil {
				t.Fatal(err)
			}
			sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
			// A command must receive its own fresh file, not an inherited path.
			sctx.Env = []string{"NO_MISTAKES_FINDINGS_FILE=do-not-use"}
			step := &CustomGateStep{Gate: config.Gate{Name: "budget", After: types.StepTest, Command: gateReportCommand(t, dir, tc.exitCode)}}
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval != tc.park || outcome.AutoFixable || outcome.ExitCode != tc.exitCode {
				t.Fatalf("outcome = %+v", outcome)
			}
			var findings Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != tc.count || !strings.Contains(findings.Summary, "gate output") {
				t.Fatalf("findings = %+v", findings)
			}
			if tc.count == 1 && findings.Items[0].ID != "budget-low" {
				t.Fatalf("finding ID = %q", findings.Items[0].ID)
			}
			if tc.name == "error on zero exit" && (findings.Items[0].File != "score.go" || findings.Items[0].Line != 7 || findings.Items[0].Action != types.ActionAutoFix) {
				t.Fatalf("finding fields = %+v", findings.Items[0])
			}
			if len(findings.Tested) != 1 || findings.Tested[0] != step.Gate.Command {
				t.Fatalf("tested command = %v", findings.Tested)
			}
			if tc.name == "warning on zero exit" && findings.Items[0].Action != types.ActionAskUser {
				t.Fatalf("missing action = %q, want ask-user", findings.Items[0].Action)
			}
			pathBytes, err := os.ReadFile(filepath.Join(dir, "gate-path.txt"))
			if err != nil {
				t.Fatal(err)
			}
			path := strings.TrimSpace(string(pathBytes))
			if !filepath.IsAbs(path) || preparationGateInsideWorktree(dir, path) {
				t.Fatalf("findings path %q must be absolute and outside %q", path, dir)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("findings file was not cleaned up: %v", err)
			}
			if len(sctx.Env) != 1 || sctx.Env[0] != "NO_MISTAKES_FINDINGS_FILE=do-not-use" {
				t.Fatalf("step environment mutated: %v", sctx.Env)
			}
		})
	}
}

func TestCustomGateStep_InvalidFindingsFailClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		report  string
		problem string
	}{
		{"malformed JSON", "{", "parse findings JSON"},
		{"whitespace only", " \n", "parse findings JSON"},
		{"missing array", `{}`, "missing findings array"},
		{"null array", `{"findings":null}`, "missing findings array"},
		{"wrong array type", `{"findings":{}}`, "parse findings JSON"},
		{"legacy items only", `{"items":[]}`, "unknown field"},
		{"unknown report field", `{"findings":[],"summary":"not part of this protocol"}`, "unknown field"},
		{"unknown finding field", `{"findings":[{"id":"x","severity":"error","description":"low score","source":"user"}]}`, "unknown field"},
		{"legacy action field", `{"findings":[{"id":"x","severity":"error","description":"low score","requires_human_review":false}]}`, "unknown field"},
		{"trailing JSON", `{"findings":[]} {}`, "parse findings JSON"},
		{"missing ID", `{"findings":[{"severity":"error","description":"low score"}]}`, "missing or duplicate id"},
		{"duplicate IDs", `{"findings":[{"id":"x","severity":"error","description":"low score"},{"id":" x ","severity":"info","description":"same ID"}]}`, "missing or duplicate id"},
		{"comma in ID", `{"findings":[{"id":"lint,style","severity":"error","description":"low score"}]}`, "id containing a comma"},
		{"protected path ID", `{"findings":[{"id":"protected-path-refusal","severity":"error","description":"low score"}]}`, "reserved id"},
		{"test timeout ID", `{"findings":[{"id":"test-agent-timeout","severity":"error","description":"low score"}]}`, "reserved id"},
		{"unvalidated work ID", `{"findings":[{"id":"test-agent-unvalidated-work","severity":"error","description":"low score"}]}`, "reserved id"},
		{"unreadable questions ID", `{"findings":[{"id":"review-questions-unreadable","severity":"error","description":"low score"}]}`, "reserved id"},
		{"missing severity", `{"findings":[{"id":"x","description":"low score"}]}`, "invalid severity"},
		{"unknown severity", `{"findings":[{"id":"x","severity":"fatal","description":"low score"}]}`, "invalid severity"},
		{"case-folded severity", `{"findings":[{"id":"x","severity":"Error","description":"low score"}]}`, "invalid severity"},
		{"padded severity", `{"findings":[{"id":"x","severity":" error ","description":"low score"}]}`, "invalid severity"},
		{"missing description", `{"findings":[{"id":"x","severity":"error"}]}`, "missing a description"},
		{"unknown action", `{"findings":[{"id":"x","severity":"error","description":"low score","action":"approve"}]}`, "invalid action"},
		{"case-folded action", `{"findings":[{"id":"x","severity":"error","description":"low score","action":"Ask-User"}]}`, "invalid action"},
		{"padded action", `{"findings":[{"id":"x","severity":"error","description":"low score","action":" ask-user "}]}`, "invalid action"},
		{"null action", `{"findings":[{"id":"x","severity":"error","description":"low score","action":null}]}`, "invalid action"},
		{"negative line", `{"findings":[{"id":"x","severity":"error","description":"low score","line":-1}]}`, "negative line"},
		{"over byte cap", strings.Repeat(" ", (1<<20)+1), "exceeds 1 MiB"},
		{"over count cap", gateFindingsReport(501), "exceeds 500 findings"},
		{"transport expansion", `{"findings":[{"id":"long","severity":"error","description":"` + strings.Repeat("<", 200000) + `"}]}`, "too large to transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			if err := os.WriteFile(filepath.Join(dir, "gate-report.json"), []byte(tc.report), 0o600); err != nil {
				t.Fatal(err)
			}
			sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
			step := &CustomGateStep{Gate: config.Gate{Name: "budget", After: types.StepTest, Command: gateReportCommand(t, dir, 0)}}
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome = %+v, want a human decision", outcome)
			}
			var findings Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatal(err)
			}
			pathBytes, err := os.ReadFile(filepath.Join(dir, "gate-path.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(strings.TrimSpace(string(pathBytes))); !os.IsNotExist(err) {
				t.Fatalf("invalid report was not cleaned up: %v", err)
			}
			if len(findings.Items) != 1 || findings.Items[0].Severity != "error" || findings.Items[0].Action != types.ActionAskUser || !strings.Contains(findings.Items[0].Description, tc.problem) {
				t.Fatalf("findings = %+v, want one parse-problem error", findings)
			}
			if tc.name == "transport expansion" {
				limit := ipc.MaxFrameBytes/2 - gateGetRunEnvelopeReserveBytes - gateTransportRefusalReserveBytes()
				if len(tc.report) != 200064 || !strings.Contains(findings.Items[0].Description, fmt.Sprintf("limit %d bytes", limit)) || findings.Summary != "" || len(findings.Tested) != 0 {
					t.Fatalf("transport refusal = %+v", findings)
				}
				frame, err := json.Marshal(ipc.Event{Type: ipc.EventStepCompleted, RunID: "run-1", RepoID: "repo-1", Findings: &outcome.Findings})
				if err != nil {
					t.Fatal(err)
				}
				scanner := bufio.NewScanner(strings.NewReader(string(frame) + "\n"))
				scanner.Buffer(make([]byte, 0, ipc.MaxFrameBytes), ipc.MaxFrameBytes)
				if !scanner.Scan() || scanner.Err() != nil {
					t.Fatalf("refusal frame broke the IPC scanner: %v", scanner.Err())
				}
			} else if !strings.Contains(findings.Summary, "gate output") {
				t.Fatalf("command output missing: %+v", findings)
			}
		})
	}
}

func TestCustomGateStep_TransportBudgetIncludesPersistedFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
	const refusingGates = 8
	sctx.Config.Gates = make([]config.Gate, refusingGates+1)
	for i := range sctx.Config.Gates {
		sctx.Config.Gates[i] = config.Gate{Name: fmt.Sprintf("budget-%d", i), After: types.StepTest, Command: gateReportCommand(t, dir, 0)}
	}
	descriptionBytes := ipc.MaxFrameBytes/2 - gateGetRunEnvelopeReserveBytes - len(sctx.Config.Gates)*gateTransportRefusalReserveBytes() - 1024
	largeDescription := strings.Repeat("x", descriptionBytes)
	var firstOutcome *pipeline.StepOutcome
	var firstResultID string
	var firstStep *CustomGateStep
	for i, gate := range sctx.Config.Gates {
		result, err := sctx.DB.InsertStepResult(sctx.Run.ID, gate.StepName())
		if err != nil {
			t.Fatal(err)
		}
		sctx.StepResultID = result.ID
		report := `{"findings":[{"id":"large-` + fmt.Sprint(i) + `","severity":"info","description":"` + largeDescription + `","action":"no-op"}]}`
		if err := os.WriteFile(filepath.Join(dir, "gate-report.json"), []byte(report), 0o600); err != nil {
			t.Fatal(err)
		}
		step := &CustomGateStep{Gate: gate}
		outcome, err := step.Execute(sctx)
		if err != nil {
			t.Fatal(err)
		}
		var findings Findings
		if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if outcome.NeedsApproval || len(findings.Items) != 1 || findings.Items[0].ID != "large-0" {
				t.Fatalf("near-full first gate = %+v", findings)
			}
			firstOutcome, firstResultID, firstStep = outcome, result.ID, step
		} else if !outcome.NeedsApproval || len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser || findings.Summary != "" || len(findings.Tested) != 0 || !strings.Contains(findings.Items[0].Description, "too large to transport") {
			t.Fatalf("refusing gate %d = %+v", i, findings)
		}
		if err := sctx.DB.SetStepFindings(result.ID, outcome.Findings); err != nil {
			t.Fatal(err)
		}
	}

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	runInfo := &ipc.RunInfo{ID: sctx.Run.ID, RepoID: sctx.Run.RepoID, Branch: sctx.Run.Branch, HeadSHA: sctx.Run.HeadSHA, BaseSHA: sctx.Run.BaseSHA, Status: sctx.Run.Status}
	for _, step := range steps {
		runInfo.Steps = append(runInfo.Steps, ipc.StepResultInfo{ID: step.ID, RunID: step.RunID, StepName: step.StepName, StepOrder: step.StepOrder, Status: step.Status, FindingsJSON: step.FindingsJSON})
	}
	assertIPCResponseFits(t, &ipc.GetRunResult{Run: runInfo}, true)

	hypothetical := *runInfo
	hypothetical.Steps = append([]ipc.StepResultInfo(nil), runInfo.Steps...)
	for i := range hypothetical.Steps {
		hypothetical.Steps[i].FindingsJSON = &firstOutcome.Findings
	}
	assertIPCResponseFits(t, &ipc.GetRunResult{Run: &hypothetical}, false)

	sctx.StepResultID = firstResultID
	if err := os.WriteFile(filepath.Join(dir, "gate-report.json"), []byte(`{"findings":[{"id":"replacement","severity":"info","description":"small replacement","action":"no-op"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := firstStep.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	var replacementFindings Findings
	if err := json.Unmarshal([]byte(replacement.Findings), &replacementFindings); err != nil {
		t.Fatal(err)
	}
	if replacement.NeedsApproval || len(replacementFindings.Items) != 1 || replacementFindings.Items[0].ID != "replacement" {
		t.Fatalf("replacement counted its own persisted findings: %+v", replacementFindings)
	}
}

func TestCustomGateStep_TransportRefusalFitsAfterLargeReviewFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Gates = make([]config.Gate, config.MaxGates)
	for i := range sctx.Config.Gates {
		sctx.Config.Gates[i] = config.Gate{Name: fmt.Sprintf("budget-%d", i), After: types.StepReview, Command: gateReportCommand(t, dir, 0)}
	}

	reviewResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	reviewFindings := Findings{Items: []Finding{{
		ID:       "review-large",
		Severity: types.FindingSeverityInfo,
		Action:   types.ActionNoOp,
	}}}
	_, emptySize := encodeGateFindings(reviewFindings)
	targetSize := ipc.MaxFrameBytes - gateGetRunEnvelopeReserveBytes - configuredGateCount(sctx)*gateTransportRefusalReserveBytes()
	reviewFindings.Items[0].Description = strings.Repeat("r", targetSize-emptySize)
	reviewJSON, reviewSize := encodeGateFindings(reviewFindings)
	if reviewSize != targetSize {
		t.Fatalf("review transport size = %d, want %d", reviewSize, targetSize)
	}
	if err := sctx.DB.SetStepFindings(reviewResult.ID, reviewJSON); err != nil {
		t.Fatal(err)
	}

	for _, stepName := range types.AllSteps() {
		if stepName == types.StepReview {
			continue
		}
		if _, err := sctx.DB.InsertStepResult(sctx.Run.ID, stepName); err != nil {
			t.Fatal(err)
		}
	}
	var gateResultID string
	for i, configuredGate := range sctx.Config.Gates {
		gateResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, configuredGate.StepName())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			gateResultID = gateResult.ID
		}
	}
	gate := sctx.Config.Gates[0]
	sctx.StepResultID = gateResultID
	if err := os.WriteFile(filepath.Join(dir, "gate-report.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&CustomGateStep{Gate: gate}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser || !strings.Contains(findings.Items[0].Description, "too large to transport") {
		t.Fatalf("transport refusal = %+v", findings)
	}
	if err := sctx.DB.SetStepFindings(gateResultID, outcome.Findings); err != nil {
		t.Fatal(err)
	}

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	runInfo := &ipc.RunInfo{ID: sctx.Run.ID, RepoID: sctx.Run.RepoID, Branch: sctx.Run.Branch, HeadSHA: sctx.Run.HeadSHA, BaseSHA: sctx.Run.BaseSHA, Status: sctx.Run.Status}
	for _, step := range steps {
		runInfo.Steps = append(runInfo.Steps, ipc.StepResultInfo{ID: step.ID, RunID: step.RunID, StepName: step.StepName, StepOrder: step.StepOrder, Status: step.Status, FindingsJSON: step.FindingsJSON})
	}
	frameSize := assertIPCResponseFits(t, &ipc.GetRunResult{Run: runInfo}, true)
	minimumBoundarySize := ipc.MaxFrameBytes - gateGetRunEnvelopeReserveBytes - configuredGateCount(sctx)*gateTransportRefusalReserveBytes()
	if frameSize < minimumBoundarySize {
		t.Fatalf("GetRun boundary frame = %d bytes, want at least %d", frameSize, minimumBoundarySize)
	}
	findingsBytes := 0
	for _, step := range steps {
		if step.FindingsJSON != nil {
			encoded, _ := json.Marshal(*step.FindingsJSON)
			findingsBytes += len(encoded)
		}
	}
	if envelopeSize := frameSize - findingsBytes; envelopeSize >= gateGetRunEnvelopeReserveBytes/4 {
		t.Fatalf("GetRun envelope = %d bytes, reserve %d lacks fourfold headroom", envelopeSize, gateGetRunEnvelopeReserveBytes)
	}
}

func assertIPCResponseFits(t *testing.T, result *ipc.GetRunResult, want bool) int {
	t.Helper()
	response, err := ipc.NewResponse(1, result)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(frame) + "\n"))
	scanner.Buffer(make([]byte, 0, ipc.MaxFrameBytes), ipc.MaxFrameBytes)
	got := scanner.Scan() && scanner.Err() == nil
	if got != want {
		t.Fatalf("IPC response fit = %t, want %t (size %d, limit %d, error %v)", got, want, len(frame), ipc.MaxFrameBytes, scanner.Err())
	}
	if got {
		var decoded ipc.Response
		if err := json.Unmarshal(scanner.Bytes(), &decoded); err != nil {
			t.Fatalf("decode IPC response: %v", err)
		}
		var decodedResult ipc.GetRunResult
		if err := json.Unmarshal(decoded.Result, &decodedResult); err != nil {
			t.Fatalf("decode GetRun result: %v", err)
		}
		if decodedResult.Run == nil || result.Run == nil || decodedResult.Run.ID != result.Run.ID {
			t.Fatalf("decoded GetRun = %+v, want run %q", decodedResult.Run, result.Run.ID)
		}
	}
	return len(frame)
}

func TestCustomGateStep_MissingOrNonRegularFindingsFailClosed(t *testing.T) {
	t.Parallel()
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint(directory), func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
			unixCommand := `rm "$NO_MISTAKES_FINDINGS_FILE"; `
			windowsCommand := `del "%NO_MISTAKES_FINDINGS_FILE%"` + "\r\n"
			if directory {
				unixCommand += `mkdir "$NO_MISTAKES_FINDINGS_FILE"; `
				windowsCommand += `mkdir "%NO_MISTAKES_FINDINGS_FILE%"` + "\r\n"
			}
			command := gateTestCommand(t, dir, unixCommand+"exit 0", windowsCommand+"exit /b 0")
			step := &CustomGateStep{Gate: config.Gate{Name: "budget", After: types.StepTest, Command: command}}
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			var findings Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser || findings.Items[0].Severity != "error" {
				t.Fatalf("outcome = %+v, findings = %+v", outcome, findings)
			}
		})
	}
}

func TestCustomGateStep_EachCommandGetsAFreshFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
	if err := os.WriteFile(filepath.Join(dir, "gate-report.json"), []byte(`{"findings":[{"id":"low","severity":"error","description":"low score"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	step := &CustomGateStep{Gate: config.Gate{Name: "budget", After: types.StepTest, Command: gateReportCommand(t, dir, 0)}}
	outcome, err := step.Execute(sctx)
	if err != nil || !outcome.NeedsApproval {
		t.Fatalf("first execution = %+v, %v", outcome, err)
	}
	step.Gate.Command = "exit 0"
	outcome, err = step.Execute(sctx)
	if err != nil || outcome.NeedsApproval || outcome.Findings != "" {
		t.Fatalf("second execution = %+v, %v, want legacy clean pass", outcome, err)
	}
}

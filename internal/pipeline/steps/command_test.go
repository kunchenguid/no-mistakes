package steps

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("command step test assumes a POSIX shell")
	}
}

// A findings_json file turns a command's output into real per-line findings,
// not an opaque "exit code 1" — the core of contract #1.
func TestCommandStep_FindingsJSON_ProducesPerLineFindings(t *testing.T) {
	skipIfWindows(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	// The command writes a findings file with two per-file/per-line findings,
	// then exits non-zero (as a real linter would).
	json := `{"findings":[` +
		`{"severity":"warning","file":"Sources/A.swift","line":12,"description":"line too long"},` +
		`{"severity":"error","file":"Sources/B.swift","line":3,"description":"force unwrap"}` +
		`],"summary":"swiftlint found 2 issues"}`
	cmd := "cat > findings.json <<'EOF'\n" + json + "\nEOF\nexit 1"

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	step := &CommandStep{StepName: "swiftlint", Command: cmd, FindingsPath: "findings.json"}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected the step to gate on findings")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 2 {
		t.Fatalf("got %d findings, want 2 per-line findings (not one exit-code finding)", len(findings.Items))
	}
	got := findings.Items[0]
	if got.File != "Sources/A.swift" || got.Line != 12 || got.Severity != "warning" {
		t.Errorf("finding[0] = %+v, want per-line A.swift:12 warning", got)
	}
	if findings.Items[1].File != "Sources/B.swift" || findings.Items[1].Line != 3 {
		t.Errorf("finding[1] = %+v, want per-line B.swift:3", findings.Items[1])
	}
	// auto_fix defaults false ⇒ findings park for an agent (ask-user), not auto-fix.
	for _, f := range findings.Items {
		if f.Action != types.ActionAskUser {
			t.Errorf("finding action = %q, want %q under default auto_fix=false", f.Action, types.ActionAskUser)
		}
	}
	if outcome.AutoFixable {
		t.Error("expected AutoFixable=false under default auto_fix=false")
	}
}

// With auto_fix enabled the same findings are marked auto-fixable, consistent
// with how built-in steps express fixability.
func TestCommandStep_AutoFixMarksFindingsFixable(t *testing.T) {
	skipIfWindows(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	json := `{"findings":[{"severity":"warning","file":"a.swift","line":1,"description":"x"}]}`
	cmd := "cat > findings.json <<'EOF'\n" + json + "\nEOF\nexit 1"

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	step := &CommandStep{StepName: "swiftlint", Command: cmd, FindingsPath: "findings.json", AutoFix: true}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.AutoFixable {
		t.Error("expected AutoFixable=true with auto_fix enabled")
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAutoFix {
		t.Errorf("finding action = %v, want auto-fix", findings.Items)
	}
}

// Without a findings_json path the step falls back to exit-code mapping,
// matching the built-in lint/test steps (backward-compatible behavior).
func TestCommandStep_ExitCodeFallbackWithoutFindingsFile(t *testing.T) {
	skipIfWindows(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	step := &CommandStep{StepName: "check", Command: "echo boom >&2; exit 2"}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.ExitCode != 2 {
		t.Fatalf("outcome = %+v, want gate with exit code 2", outcome)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Items) != 1 {
		t.Fatalf("got %d findings, want 1 synthetic exit-code finding", len(findings.Items))
	}
}

// A passing command completes the step with no gate and no findings.
func TestCommandStep_PassesOnExitZero(t *testing.T) {
	skipIfWindows(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	step := &CommandStep{StepName: "check", Command: "true"}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.Findings != "" {
		t.Fatalf("outcome = %+v, want clean pass", outcome)
	}
}

// A per-step timeout bounds a long-running command and surfaces a clear timeout
// failure rather than hanging the gate.
func TestCommandStep_TimeoutSurfacesClearFailure(t *testing.T) {
	skipIfWindows(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	step := &CommandStep{StepName: "hang", Command: "sleep 30", Timeout: 150 * time.Millisecond}

	start := time.Now()
	outcome, err := step.Execute(sctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("timeout should surface as a gated outcome, not an error: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("step did not honor its timeout (took %s)", elapsed)
	}
	if !outcome.NeedsApproval {
		t.Error("expected a timeout to gate the step")
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Items) != 1 {
		t.Fatalf("got %d findings, want 1 timeout finding", len(findings.Items))
	}
	if !strings.Contains(findings.Items[0].Description, "timed out") {
		t.Errorf("finding description = %q, want it to mention the timeout", findings.Items[0].Description)
	}
	if outcome.AutoFixable {
		t.Error("a timeout should not be auto-fixable")
	}
}

// A cancelled run must fail the run, never be mistaken for a step timeout.
func TestCommandStep_RunCancellationFailsRun(t *testing.T) {
	skipIfWindows(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	ctx, cancel := context.WithCancel(context.Background())
	sctx.Ctx = ctx
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	step := &CommandStep{StepName: "hang", Command: "sleep 30", Timeout: 30 * time.Second}

	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected run cancellation to fail the step")
	}
}

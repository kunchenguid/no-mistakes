package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// attributionRepo commits a failing-suite script on main, then a feature
// commit that changes it, and returns the step context for the feature head.
// The script prints one go-test style failure line per name in failures.txt
// and exits non-zero when there is any.
func attributionRepo(t *testing.T, baseFailures, headFailures string, attribution bool) (*pipeline.StepContext, *mockAgent, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture suite is a POSIX shell script")
	}
	dir := t.TempDir()
	gitCmd(t, dir, "init", "--quiet")
	gitCmd(t, dir, "checkout", "--quiet", "-b", "main")
	script := "#!/bin/sh\nstatus=0\nwhile read -r name; do [ -n \"$name\" ] && echo \"--- FAIL: $name (0.0${RANDOM:-1}s)\" && status=1; done < failures.txt\nexit $status\n"
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("suite.sh", script)
	write("failures.txt", baseFailures)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "--quiet", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "--quiet", "-b", "feature")
	write("failures.txt", headFailures)
	gitCmd(t, dir, "commit", "--quiet", "-am", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "./suite.sh"})
	sctx.Config.Test.BaseAttribution = attribution
	return sctx, ag, dir
}

func lastPrompt(ag *mockAgent) string {
	if len(ag.calls) == 0 {
		return ""
	}
	return ag.calls[len(ag.calls)-1].Prompt
}

// A head that keeps one failure main already has and adds one of its own must
// name only the new one as introduced, while the configured-command finding,
// its approval-override reason, and the run worktree stay exactly as they are
// without attribution.
func TestTestStep_BaseAttributionSeparatesIntroducedFromPreexisting(t *testing.T) {
	t.Parallel()
	sctx, ag, dir := attributionRepo(t, "TestFlakyOnMain\n", "TestFlakyOnMain\nTestBrokenByChange\n", true)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 1 || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the failing command to park as before", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	introduced := "Introduced by this change (1):\n- --- FAIL: TestBrokenByChange\n"
	preexisting := "Pre-existing on the base commit (1):\n- --- FAIL: TestFlakyOnMain"
	for _, want := range []string{"also fails on base commit", introduced, preexisting} {
		if !strings.Contains(findings.Summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, findings.Summary)
		}
		if !strings.Contains(lastPrompt(ag), want) {
			t.Fatalf("evidence prompt missing %q:\n%s", want, lastPrompt(ag))
		}
	}
	if strings.Contains(findings.Summary, "Introduced by this change (1):\n- --- FAIL: TestFlakyOnMain") {
		t.Fatalf("pre-existing failure attributed to the change:\n%s", findings.Summary)
	}
	if len(findings.Items) == 0 || findings.Items[0].Description != "configured test command failed with exit code 1" || findings.Items[0].Severity != "error" {
		t.Fatalf("configured-command finding changed: %+v", findings.Items)
	}

	persistTestStepFindings(t, sctx, outcome.ExitCode, outcome.Findings)
	unresolved, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved != "configured test command failed with exit code 1" {
		t.Fatalf("override reason = %q, want approval to stay a configured-command waiver", unresolved)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("base run left changes in the run worktree:\n%s", status)
	}
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != sctx.Run.HeadSHA {
		t.Fatalf("run worktree HEAD = %s, want %s", head, sctx.Run.HeadSHA)
	}
}

// A suite that is green on the base commit means the change introduced every
// failure, which is stated rather than left for the reader to infer.
func TestTestStep_BaseAttributionGreenBaseBlamesTheChange(t *testing.T) {
	t.Parallel()
	sctx, _, _ := attributionRepo(t, "", "TestBrokenByChange\n", true)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(findings.Summary, "passes on base commit") || !strings.Contains(findings.Summary, "Introduced by this change (1):\n- --- FAIL: TestBrokenByChange") {
		t.Fatalf("summary = %q, want the change named as the cause", findings.Summary)
	}
	if strings.Contains(findings.Summary, "Pre-existing") {
		t.Fatalf("summary claims pre-existing failures on a green base:\n%s", findings.Summary)
	}
}

// Off by default: without the opt-in the base commit is never checked out or
// run, so the summary is the head output alone.
func TestTestStep_BaseAttributionIsOptIn(t *testing.T) {
	t.Parallel()
	sctx, _, _ := attributionRepo(t, "TestFlakyOnMain\n", "TestFlakyOnMain\nTestBrokenByChange\n", false)
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(findings.Summary, "attribution") || containsLog(logs, "re-running it on base commit") {
		t.Fatalf("attribution ran without the opt-in: summary=%q logs=%q", findings.Summary, logs)
	}
}

// Package-level aggregates fail on both sides whenever any test in the package
// does, so they must never be listed: only per-test lines are compared.
func TestTestFailureLinesNormalizesRunnerNoise(t *testing.T) {
	t.Parallel()
	output := strings.Join([]string{
		"=== RUN   TestA",
		"    --- FAIL: TestA (0.01s)",
		"\x1b[31m--- FAIL: TestA (0.42s)\x1b[0m",
		"FAIL\tgithub.com/x/y\t0.512s",
		"FAILED tests/test_api.py::test_login - AssertionError",
		"not ok 7 - parses empty input",
		"test parser::empty ... FAILED",
		"ok  \tgithub.com/x/z\t0.1s",
		"PASS",
	}, "\n")
	got := testFailureLines(output)
	want := []string{
		"--- FAIL: TestA",
		"FAILED tests/test_api.py::test_login - AssertionError",
		"not ok - parses empty input",
		"test parser::empty ... FAILED",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("testFailureLines() = %q, want %q", got, want)
	}
}

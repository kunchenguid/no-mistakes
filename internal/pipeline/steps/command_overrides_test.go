package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func commandOverrideContext(t *testing.T, override config.CommandOverride) *pipeline.StepContext {
	t.Helper()
	return &pipeline.StepContext{
		Ctx: context.Background(), WorkDir: t.TempDir(), Log: func(s string) { t.Log(s) },
		Config: &config.Config{CommandOverrides: map[string]config.CommandOverride{"test": override}},
	}
}

func TestRepositoryCommand_NoOverrideKeepsOutputAndExitCode(t *testing.T) {
	sctx := commandOverrideContext(t, config.CommandOverride{})
	sctx.Config.CommandOverrides = nil
	for _, command := range []string{"echo unchanged", "exit 7"} {
		want, wantCode, wantErr := runStepShellCommand(sctx, command)
		got, results, err := runConfiguredChecks(sctx, "test", command)
		if got != want || len(results) != 1 || results[0].Local || results[0].ExitCode != wantCode || err != wantErr {
			t.Fatalf("command %q = (%q, %+v, %v), want (%q, %d, %v)", command, got, results, err, want, wantCode, wantErr)
		}
	}
}

func TestConfiguredChecks_AddWithoutMaskingEitherFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		teamExit  int
		localExit int
		wantExit  int
	}{{"both pass", 0, 0, 0}, {"team fails", 7, 0, 7}, {"local fails", 0, 9, 9}, {"both fail", 7, 9, 7}} {
		t.Run(tc.name, func(t *testing.T) {
			separator := "; "
			if runtime.GOOS == "windows" {
				separator = " & "
			}
			local := "echo local>>order.txt" + separator + "exit " + strconv.Itoa(tc.localExit)
			team := "echo team>>order.txt" + separator + "exit " + strconv.Itoa(tc.teamExit)
			sctx := commandOverrideContext(t, config.CommandOverride{Additional: []string{local}})
			_, results, err := runConfiguredChecks(sctx, "test", team)
			if err != nil || len(results) != 2 || results[0].ExitCode != tc.teamExit || results[0].Local || results[1].ExitCode != tc.localExit || !results[1].Local {
				t.Fatalf("results = %+v, %v", results, err)
			}
			if failed := failedChecks(results); tc.wantExit != 0 && failed[0].ExitCode != tc.wantExit {
				t.Fatalf("first failure = %+v, want exit %d", failed, tc.wantExit)
			}
			data, err := os.ReadFile(filepath.Join(sctx.WorkDir, "order.txt"))
			if err != nil || strings.ReplaceAll(string(data), "\r\n", "\n") != "team\nlocal\n" {
				t.Fatalf("checks did not run separately in order: %q, %v", data, err)
			}
		})
	}
}

func TestRepositoryCommand_RemapsToolchainWithoutRewritingCommand(t *testing.T) {
	bin := t.TempDir()
	name, script := "nm-local-tool", "#!/bin/sh\nprintf '%s:%s' \"$1\" \"$NM_PARALLEL\"\n"
	if runtime.GOOS == "windows" {
		name, script = "nm-local-tool.cmd", "@echo off\r\necho %1:%NM_PARALLEL%\r\n"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sctx := commandOverrideContext(t, config.CommandOverride{Env: map[string]string{
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "NM_PARALLEL": "2",
	}})
	sctx.Env = []string{"NM_PARALLEL=original"}
	command := "nm-local-tool all-tests"
	out, code, err := runRepositoryCommand(sctx, "test", command)
	if err != nil || code != 0 || strings.TrimSpace(out) != "all-tests:2" {
		t.Fatalf("remapped command = (%q, %d, %v)", out, code, err)
	}
	if got, _ := envValue(stepEnvironment(sctx), "NM_PARALLEL"); got != "original" {
		t.Fatal("command environment leaked into other step subprocesses")
	}
}

func TestRepositoryCommand_EnvironmentCannotUndoForgeIdentity(t *testing.T) {
	sctx := commandOverrideContext(t, config.CommandOverride{Env: map[string]string{"GH_CONFIG_DIR": "wrong-profile", "GH_TOKEN": "wrong-token"}})
	sctx.ForgeContext = &forgecontext.Context{Environment: runenv.Overlay{Set: map[string]string{"GH_CONFIG_DIR": "pinned-profile"}, Unset: []string{"GH_TOKEN"}}}
	command := "printf '%s:%s' \"$GH_CONFIG_DIR\" \"$GH_TOKEN\""
	if runtime.GOOS == "windows" {
		command = "echo %GH_CONFIG_DIR%:%GH_TOKEN%"
	}
	out, code, err := runRepositoryCommand(sctx, "test", command)
	if err != nil || code != 0 || strings.Contains(out, "wrong") || !strings.Contains(out, "pinned-profile") {
		t.Fatalf("forge identity = (%q, %d, %v)", out, code, err)
	}
}

func TestRepositoryCommand_LowersOSPriority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("nice is a POSIX scheduling knob")
	}
	sctx := commandOverrideContext(t, config.CommandOverride{})
	base, code, err := runRepositoryCommand(sctx, "test", "nice")
	if err != nil || code != 0 {
		t.Fatalf("read baseline niceness: %q %d %v", base, code, err)
	}
	baseNice, err := strconv.Atoi(strings.TrimSpace(base))
	if err != nil {
		t.Fatal(err)
	}
	sctx.Config.CommandOverrides["test"] = config.CommandOverride{Nice: 1}
	out, code, err := runRepositoryCommand(sctx, "test", "nice")
	if err != nil || code != 0 || strings.TrimSpace(out) != strconv.Itoa(min(baseNice+1, 19)) {
		t.Fatalf("niceness = (%q, %d, %v), baseline %d", out, code, err, baseNice)
	}
}

func TestTestStep_LocalCheckFailureCannotBecomeAGreenBaseline(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["live command"],"testing_summary":"live command exercised","artifacts":[],"scenarios":[{"name":"command","result":"pass","live":true,"evidence":"observed","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{Test: "exit 0"})
	sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Additional: []string{"exit 9"}}}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || !outcome.NeedsApproval || outcome.ExitCode != 9 || len(ag.calls) != 1 {
		t.Fatalf("local failure must park even after live evidence: %+v, %v", outcome, err)
	}
	if len(findings.Items) == 0 || findings.Items[0].Category != types.FindingCategoryTestCommand {
		t.Fatalf("failure did not retain configured-command gating: %+v", findings)
	}
	if !strings.Contains(outcome.Findings, "exit 9") {
		t.Fatal("local check missing from Test evidence")
	}
	for _, item := range findings.Items {
		if strings.Contains(item.Description, "configured test command failed") {
			t.Fatalf("passing team command blamed for the local check: %+v", findings.Items)
		}
	}
	if findings.Items[0].Description != "machine-local test check failed with exit code 9: exit 9" {
		t.Fatalf("local failure not attributed to the check by name: %q", findings.Items[0].Description)
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "Configured test command already ran successfully as baseline: `exit 0`") ||
		!strings.Contains(prompt, "failed with exit code 9: `exit 9`") ||
		strings.Contains(prompt, "Configured test command failed") {
		t.Fatalf("prompt misattributes the baseline:\n%s", prompt)
	}
}

func TestTestStep_LocalCheckFailureWithoutTeamCommandReachesTheAgentByName(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["live command"],"testing_summary":"live command exercised","artifacts":[],"scenarios":[{"name":"command","result":"pass","live":true,"evidence":"observed","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Additional: []string{"exit 5"}}}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || !outcome.NeedsApproval || outcome.ExitCode != 5 || len(findings.Items) == 0 {
		t.Fatalf("local failure lost: %+v, %v", outcome, err)
	}
	if findings.Items[0].Description != "machine-local test check failed with exit code 5: exit 5" {
		t.Fatalf("finding = %q", findings.Items[0].Description)
	}
	if !strings.Contains(ag.calls[0].Prompt, "failed with exit code 5: `exit 5`") {
		t.Fatalf("failed local check missing from the agent prompt:\n%s", ag.calls[0].Prompt)
	}
	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, "no test command configured") || !strings.Contains(joined, "machine-local test check failed: exit 5") {
		t.Fatalf("logs misreport the failure:\n%s", joined)
	}
}

func TestTestStep_OverriddenPassIsDeclared(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["live command"],"testing_summary":"live command exercised","artifacts":[],"scenarios":[{"name":"command","result":"pass","live":true,"evidence":"observed","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{Test: "exit 0"})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Env: map[string]string{"GOFLAGS": "-p=2"}, Additional: []string{"exit 0"}}}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil || outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	want := `machine-local overrides applied to commands.test: env GOFLAGS="-p=2"; additional checks "exit 0"`
	if !strings.Contains(strings.Join(logs, "\n"), want) {
		t.Fatalf("step output does not declare the overrides:\n%s", strings.Join(logs, "\n"))
	}
	if !strings.Contains(ag.calls[0].Prompt, `machine-local overrides applied to commands.test: env GOFLAGS; additional checks "exit 0"`) {
		t.Fatal("agent prompt does not declare the overrides")
	}
}

func TestTestStep_OverrideEnvValuesNeverReachThePromptOrFindings(t *testing.T) {
	const secret = "postgres://user:s3cret@internal-db"
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["live command"],"testing_summary":"live command exercised","artifacts":[],"scenarios":[{"name":"command","result":"pass","live":true,"evidence":"observed","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{Test: "exit 0"})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Env: map[string]string{"DATABASE_URL": secret}, Additional: []string{"exit 3"}}}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	prompt := ag.calls[0].Prompt
	if strings.Contains(prompt, secret) || !strings.Contains(prompt, "env DATABASE_URL") {
		t.Fatalf("prompt must declare the key without its value:\n%s", prompt)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outcome.Findings, secret) || strings.Contains(findings.TestingSummary, secret) || strings.Contains(findings.Summary, secret) {
		t.Fatalf("override value published in Test findings: %s", outcome.Findings)
	}
	if !strings.Contains(strings.Join(logs, "\n"), `DATABASE_URL="`+secret+`"`) {
		t.Fatal("private step log lost the exact override value")
	}
}

func TestRepositoryCommand_DeclaresOverridesForEveryCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("nice is a POSIX scheduling knob")
	}
	for _, name := range []string{"prepare", "format", "lint", "test"} {
		var logs []string
		sctx := commandOverrideContext(t, config.CommandOverride{})
		sctx.Log = func(s string) { logs = append(logs, s) }
		sctx.Config.CommandOverrides = map[string]config.CommandOverride{name: {Env: map[string]string{"B": "2", "A": "1"}, Nice: 3}}
		if _, code, err := runRepositoryCommand(sctx, name, "exit 0"); err != nil || code != 0 {
			t.Fatalf("%s: %d %v", name, code, err)
		}
		want := "machine-local overrides applied to commands." + name + `: env A="1" B="2"; nice 3`
		if len(logs) != 1 || logs[0] != want {
			t.Fatalf("%s logs = %q, want %q", name, logs, want)
		}
	}
	var logs []string
	sctx := commandOverrideContext(t, config.CommandOverride{})
	sctx.Config.CommandOverrides = nil
	sctx.Log = func(s string) { logs = append(logs, s) }
	if _, _, err := runRepositoryCommand(sctx, "test", "exit 0"); err != nil || len(logs) != 0 {
		t.Fatalf("no override must declare nothing: %q, %v", logs, err)
	}
}

func TestLintStep_AddedChecksDoNotReplaceAgentOnlyLint(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{})
	sctx.Config.CommandOverrides = map[string]config.CommandOverride{"lint": {Additional: []string{"exit 9"}}}
	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 || !outcome.NeedsApproval || outcome.ExitCode != 9 {
		t.Fatalf("agent duty or local failure lost: calls=%d outcome=%+v", len(ag.calls), outcome)
	}
}

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
		got, code, err := runConfiguredChecks(sctx, "test", configuredCheckCommands(sctx, "test", command))
		if got != want || code != wantCode || err != wantErr {
			t.Fatalf("command %q = (%q, %d, %v), want (%q, %d, %v)", command, got, code, err, want, wantCode, wantErr)
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
			_, code, err := runConfiguredChecks(sctx, "test", configuredCheckCommands(sctx, "test", team))
			if err != nil || code != tc.wantExit {
				t.Fatalf("exit = %d, %v", code, err)
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

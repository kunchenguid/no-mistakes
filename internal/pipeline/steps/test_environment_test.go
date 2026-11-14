package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMissingTool_ConfiguredCommandParksForSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell exit 126/127 fixtures")
	}
	t.Parallel()
	for _, tc := range []struct {
		name string
		code int
		mode os.FileMode
	}{
		{"absent", 127, 0},
		{"not-executable", 126, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			tool := filepath.Join(t.TempDir(), "missing-tool-fixture")
			if tc.mode != 0 {
				if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if strings.HasPrefix(opts.Prompt, "Fix the failing tests") {
					t.Fatal("setup failure must not start a repair turn")
				}
				if !strings.Contains(opts.Prompt, "commands.prepare") || !strings.Contains(opts.Prompt, "Do not install missing tools or dependencies") {
					t.Error("evidence turn needs missing-dependency setup guidance")
				}
				// Even a green evidence verdict cannot erase the configured failure.
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: fmt.Sprintf("'%s'", tool)})
			sctx.Config.Test.BaseAttribution = true
			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || outcome.AutoFixable || outcome.Skipped || outcome.ExitCode != tc.code {
				t.Fatalf("outcome = %+v, want parked setup failure retaining exit %d without automatic repair", outcome, tc.code)
			}
			if calls != 1 {
				t.Fatalf("agent calls = %d, want the unconditional evidence turn only", calls)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryTestCommand || findings.Items[0].Action != types.ActionAskUser || !strings.Contains(findings.Items[0].Description, "commands.prepare") {
				t.Fatalf("findings = %+v, want retained configured-command failure with human setup guidance", findings.Items)
			}
			if !slices.Contains(findings.Tested, sctx.Config.Commands.Test) || !strings.Contains(findings.Summary, "missing-tool-fixture") {
				t.Fatalf("configured command and its output were lost: %+v", findings)
			}
			persistTestStepFindings(t, sctx, outcome.ExitCode, outcome.Findings)
			if reason, err := (&TestStep{}).VerifyApprovalOverride(sctx); err != nil || reason != findings.Items[0].Description || !strings.HasPrefix(reason, fmt.Sprintf("configured test command failed with exit code %d", tc.code)) {
				t.Fatalf("approval override = %q, %v, want the failed test preserved", reason, err)
			}
			if _, err := os.Stat(tool); tc.mode == 0 && !os.IsNotExist(err) {
				t.Fatalf("missing tool was materialized: %v", err)
			}
		})
	}
}

func TestMissingTool_BaseAttributionStillRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell exit 126/127 fixtures")
	}
	t.Parallel()
	for _, tc := range []struct {
		name string
		code int
		mode os.FileMode
	}{
		{"absent", 127, 0},
		{"not-executable", 126, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			fixtures := t.TempDir()
			tool := filepath.Join(fixtures, "missing-tool-fixture")
			trace := filepath.Join(fixtures, "executions")
			if tc.mode != 0 {
				if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}}
			command := fmt.Sprintf("pwd >> '%s'; '%s'", trace, tool)
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: command})
			sctx.Config.Test.BaseAttribution = true
			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || outcome.AutoFixable || outcome.Skipped || outcome.ExitCode != tc.code {
				t.Fatalf("attribution changed the setup gate: %+v", outcome)
			}
			executions, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			checkouts := strings.Split(strings.TrimSuffix(string(executions), "\n"), "\n")
			if len(checkouts) != 2 || checkouts[0] == checkouts[1] {
				t.Fatalf("command execution directories = %q, want the head and a separate base checkout", executions)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("Base attribution unavailable (test command could not run on the base checkout (exit code %d))", tc.code)
			if !strings.Contains(findings.Summary, want) {
				t.Fatalf("summary = %q, want %q", findings.Summary, want)
			}
			if len(ag.calls) != 1 || !strings.Contains(ag.calls[0].Prompt, want) {
				t.Fatalf("agent calls = %+v, want one evidence turn carrying the base attribution", ag.calls)
			}
		})
	}
}

func TestMissingTool_OrdinaryConfiguredCommandKeepsRepairFlow(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	repairs, evidence := 0, 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.HasPrefix(opts.Prompt, "Fix the failing tests") {
			repairs++
			if !strings.Contains(opts.Prompt, "Do not install missing tools or dependencies") || !strings.Contains(opts.Prompt, "commands.prepare") {
				t.Error("repair turn needs missing-dependency setup guidance")
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"no changes needed in this fixture"}`)}, nil
		}
		evidence++
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: "exit 1"})
	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 1 {
		t.Fatalf("ordinary failure lost repair flow: %+v", outcome)
	}
	sctx.Fixing, sctx.PreviousFindings = true, outcome.Findings
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if repairs != 1 || evidence != 2 {
		t.Fatalf("repair calls = %d, evidence calls = %d, want 1 and 2", repairs, evidence)
	}
}

func TestMissingTool_PrepareMaterializesToolForLaterCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if !strings.Contains(strings.ToLower(opts.Prompt), "do not remove dependencies materialized by commands.prepare") {
			t.Fatal("evidence cleanup must preserve prepared dependencies")
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newPreparationTestContext(t, ag, dir, base, head, config.Commands{
		Prepare: `mkdir -p .deps && printf '#!/bin/sh\nprintf "prepared tool ran\\n"\n' > .deps/tool && chmod +x .deps/tool`,
		Test:    "./.deps/tool",
	})
	sctx.Shared = &pipeline.RunShared{}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 || calls != 1 {
		t.Fatalf("prepared tool did not pass: %+v, agent calls = %d", outcome, calls)
	}
	output, code, err := runRepositoryCommand(sctx, "test", "./.deps/tool")
	if err != nil || code != 0 || !strings.Contains(output, "prepared tool ran") {
		t.Fatalf("prepared tool did not survive for the next command: %q, %d, %v", output, code, err)
	}
}

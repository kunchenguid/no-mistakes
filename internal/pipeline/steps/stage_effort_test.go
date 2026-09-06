package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Real executor + real ReviewStep + real Pi adapter + recording native fixture.
// This is deterministic integration evidence, NOT a credentialed model smoke.
func TestReviewLoop_StageEffortNativeReviewFixRereview(t *testing.T) {
	for _, tc := range []struct {
		name               string
		failResume, legacy bool
	}{
		{name: "legacy global effort", legacy: true},
		{name: "successful resume"},
		{name: "failed resume", failResume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failResume := tc.failResume
			_, database, run, repo, workDir := reviewSessionHarness(t, &sessionMockAgent{}, nil)
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "pi")
			bin := filepath.Join(binDir, "pi")
			if _, err := os.Stat(bin); err != nil {
				bin += ".exe"
			}
			log := filepath.Join(t.TempDir(), "argv.log")
			env := map[string]string{"FAKE_CLI_MODE": "pi-effort", "FAKE_CLI_LOG": log, "FAKE_PI_REVIEW_COUNT": filepath.Join(t.TempDir(), "review-count")}
			if failResume {
				env["FAKE_PI_FAIL_RESUME"] = "1"
			}
			stages := agentcfg.StageEfforts{"review": "high", "review-fix": "medium"}
			if tc.legacy {
				stages = nil
			}
			ag, err := agent.NewWithOptions(types.AgentPi, bin, nil, agent.Options{Profile: agentcfg.Profile{Model: "fixture-model", Effort: "medium"}, StageEfforts: stages, Environment: runenv.Overlay{Set: env}})
			if err != nil {
				t.Fatal(err)
			}
			defer ag.Close()
			cfg := &config.Config{Agent: types.AgentPi, AutoFix: config.AutoFix{Review: 3}, SessionReuse: true}
			exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, ag, []pipeline.Step{&ReviewStep{}}, nil)
			if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			want := []string{"high", "medium", "high", "medium", "high"}
			if failResume {
				want = []string{"high", "medium", "high", "medium", "medium", "high"}
			}
			if tc.legacy {
				for i := range want {
					want[i] = "medium"
				}
			}
			if len(lines) != len(want) {
				t.Fatalf("invocations = %s", data)
			}
			for i, line := range lines {
				if !strings.Contains(line, "--thinking "+want[i]) || !strings.Contains(line, "--model fixture-model") {
					t.Errorf("invocation %d = %s", i, line)
				}
				if want[i] == "high" && !strings.Contains(line, "--no-session") {
					t.Errorf("review reused fixer session: %s", line)
				}
			}
			if !strings.Contains(lines[3], "--session 019ff2f3") {
				t.Fatalf("fix did not resume: %s", data)
			}
			if failResume && strings.Contains(lines[4], "--session") {
				t.Fatalf("fresh fallback retained session: %s", data)
			}
			t.Logf("recording Pi fixture native argv (not credentialed):\n%s", data)
		})
	}
}

func TestHousekeeping_StageEffortsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stages agentcfg.StageEfforts
		raw    []string
		want   []string
	}{
		{name: "default", want: []string{"housekeeping"}},
		{name: "equal", stages: agentcfg.StageEfforts{"document": "high", "lint": "high"}, want: []string{"housekeeping"}},
		{name: "different", stages: agentcfg.StageEfforts{"document": "high", "lint": "low"}, want: []string{"document", "lint"}},
		{name: "inherited", stages: agentcfg.StageEfforts{"lint": "high"}, want: []string{"document", "lint"}},
		{name: "raw pin", stages: agentcfg.StageEfforts{"lint": "high"}, raw: []string{"--thinking", "low"}, want: []string{"housekeeping"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "pi", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean"}`)}, nil
			}}
			sctx := newHousekeepingContext(t, ag, dir, base, head, config.Commands{})
			sctx.Config.Agent = types.AgentPi
			sctx.Config.StageEffort = map[string]agentcfg.StageEfforts{"pi": tc.stages}
			sctx.Config.AgentArgsOverride = map[string][]string{"pi": tc.raw}
			for _, step := range []pipeline.Step{&DocumentStep{}, &LintStep{}} {
				if _, err := step.Execute(sctx); err != nil {
					t.Fatal(err)
				}
			}
			if len(ag.calls) != len(tc.want) {
				t.Fatalf("calls = %d, want %v", len(ag.calls), tc.want)
			}
			for i, call := range ag.calls {
				if call.Purpose != tc.want[i] {
					t.Errorf("call %d purpose = %q", i, call.Purpose)
				}
			}
			if _, ok := sctx.Shared.TakeHousekeepingLint(); ok {
				t.Fatal("stale housekeeping result remains")
			}
		})
	}
}

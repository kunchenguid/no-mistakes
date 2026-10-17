package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const evidenceBoundGoFindings = `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"reader opens the page","result":"untested","live":false,"evidence":"","reason":"docs only"}],"verdict":"no-surface"}`

// TestTestStep_EvidenceTurnBoundOnlyWithoutRuntimeSurface proves the bound in
// both directions: a change whose every path is documentation, tests, or CI
// workflows gets the short evidence budget and says so in the step log, and
// any change that touches a runtime path - committed or still untracked - keeps
// the full test_agent_timeout. A fix round answering a budget cut gets the full
// budget back even on a no-surface change.
func TestTestStep_EvidenceTurnBoundOnlyWithoutRuntimeSurface(t *testing.T) {
	t.Parallel()
	const configured = 30 * time.Minute
	cases := []struct {
		name      string
		files     map[string]string
		untracked map[string]string
		fixingCut bool
		wantBound bool
	}{
		{name: "docs tests and workflows are bounded", files: map[string]string{
			"README.md":                  "docs\n",
			"docs/guide.mdx":             "docs\n",
			"internal/foo/foo_test.go":   "package foo\n",
			"tests/fixtures/input.json":  "{}\n",
			".github/workflows/ci.yml":   "on: push\n",
			"internal/foo/testdata/a.go": "package a\n",
		}, wantBound: true},
		{name: "one runtime path keeps the full budget", files: map[string]string{
			"README.md":         "docs\n",
			"internal/foo/a.go": "package foo\n",
		}},
		{name: "config and composite actions are runtime", files: map[string]string{
			".github/actions/x/action.yml": "runs: {}\n",
		}},
		{name: "plain text data is runtime", files: map[string]string{
			"prompts/system.txt": "be nice\n",
		}},
		{name: "untracked runtime file keeps the full budget", files: map[string]string{
			"README.md": "docs\n",
		}, untracked: map[string]string{"main.go": "package main\n"}},
		{name: "fix round answering a budget cut gets the full budget", files: map[string]string{
			"README.md": "docs\n",
		}, fixingCut: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := setupGitRepo(t)
			gitCmd(t, dir, "reset", "-q", "--hard", baseSHA)
			writeFiles(t, dir, tc.files)
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-q", "-m", "change")
			headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
			writeFiles(t, dir, tc.untracked)

			var mu sync.Mutex
			var budgets []time.Duration
			var prompts []string
			ag := &mockAgent{name: "bound-probe", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Error("evidence turn ran without a deadline")
				}
				mu.Lock()
				budgets = append(budgets, time.Until(deadline))
				prompts = append(prompts, opts.Prompt)
				mu.Unlock()
				if strings.Contains(opts.Prompt, "Fix the failing tests") {
					return &agent.Result{Output: json.RawMessage(`{"summary":"no changes"}`)}, nil
				}
				return &agent.Result{Output: json.RawMessage(evidenceBoundGoFindings)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.TestAgentTimeout = configured
			var logs []string
			sctx.Log = func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }
			if tc.fixingCut {
				sctx.Fixing = true
				sctx.PreviousFindings = `{"findings":[{"id":"` + types.FindingIDTestAgentTimeout + `","severity":"warning","action":"ask-user","description":"cut"}]}`
			}

			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(budgets) == 0 {
				t.Fatal("evidence agent never ran")
			}
			evidence := budgets[len(budgets)-1]
			bounded := evidence <= noSurfaceEvidenceBound
			if bounded != tc.wantBound {
				t.Fatalf("evidence budget = %s, want bounded=%v (bound %s, configured %s)", evidence, tc.wantBound, noSurfaceEvidenceBound, configured)
			}
			if !tc.wantBound && evidence < configured-time.Minute {
				t.Fatalf("evidence budget = %s, want the full configured %s", evidence, configured)
			}
			joined := strings.Join(logs, "\n")
			named := strings.Contains(joined, "no runtime surface") && strings.Contains(joined, noSurfaceEvidenceBound.String())
			if named != tc.wantBound {
				t.Fatalf("bound visible in step log = %v, want %v; logs:\n%s", named, tc.wantBound, joined)
			}
			inPrompt := strings.Contains(prompts[len(prompts)-1], "this evidence turn is bounded to")
			if inPrompt != tc.wantBound {
				t.Fatalf("bound named in evidence prompt = %v, want %v", inPrompt, tc.wantBound)
			}
		})
	}
}

// TestTestStep_NoSurfaceBoundNeverShortensASmallerBudget keeps the bound a cap:
// an operator budget already below it is used as configured, and its cut is
// the ordinary budget-cut park with no no-surface claim.
func TestTestStep_NoSurfaceBoundNeverShortensASmallerBudget(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	gitCmd(t, dir, "reset", "-q", "--hard", baseSHA)
	writeFiles(t, dir, map[string]string{"README.md": "docs\n"})
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "-m", "docs")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	ag := &mockAgent{name: "hang", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || len(findings.Items) == 0 || findings.Items[0].ID != types.FindingIDTestAgentTimeout {
		t.Fatalf("outcome = %+v, want the budget-cut park", outcome)
	}
	if strings.Contains(findings.Items[0].Description, "no runtime surface") {
		t.Fatalf("a configured budget below the bound was reported as the no-surface bound: %s", findings.Items[0].Description)
	}
}

// TestEvidenceBudgetCutSettingNamesOnlyTheLimitThatCut covers the case where
// the bound shortens only test_agent_working_timeout (for example a 10m silent
// budget with a 30m still-working cap): a cut by the operator's own silent
// budget must stay the ordinary budget cut, and only a cut at the shortened
// cap may claim the no-surface bound. The errors come from the real agent-run
// seam, so the classification reads the same typed cut the step receives.
func TestEvidenceBudgetCutSettingNamesOnlyTheLimitThatCut(t *testing.T) {
	t.Parallel()
	const silent = 20 * time.Millisecond
	const workingCap = 80 * time.Millisecond
	cut := func(t *testing.T, streaming bool) error {
		t.Helper()
		ag := &mockAgent{name: "cut", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			tick := time.NewTicker(2 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-tick.C:
					if streaming {
						opts.OnChunk("working\n")
					}
				}
			}
		}}
		sctx := &pipeline.StepContext{Ctx: context.Background(), Agent: ag}
		_, err := sctx.RunAgentBudget(sctx.Ctx, silent, workingCap, errTestAgentTimeout, agent.RunOpts{OnChunk: func(string) {}})
		if !errors.Is(err, errTestAgentTimeout) {
			t.Fatalf("err = %v, want a Test budget cut", err)
		}
		return err
	}
	workingOnly := testEvidenceBudget{timeout: silent, working: workingCap, shortWorking: true}

	if got := workingOnly.cutSetting(cut(t, false)); got != "" {
		t.Fatalf("silent-budget cut with only the cap shortened = %q, want the ordinary budget cut", got)
	}
	if got := workingOnly.cutSetting(cut(t, true)); got != "test_agent_working_timeout" {
		t.Fatalf("cut at the shortened cap = %q, want test_agent_working_timeout", got)
	}
	// A cap shortened down to the silent budget no longer extends the turn,
	// so every cut is the bound's.
	collapsed := testEvidenceBudget{timeout: silent, working: silent, shortWorking: true}
	if got := collapsed.cutSetting(cut(t, false)); got != "test_agent_working_timeout" {
		t.Fatalf("cut with the extension removed by the bound = %q, want test_agent_working_timeout", got)
	}
	both := testEvidenceBudget{timeout: silent, working: silent, shortTimeout: true, shortWorking: true}
	if got := both.cutSetting(cut(t, false)); got != "test_agent_timeout and test_agent_working_timeout" {
		t.Fatalf("cut with both limits bounded = %q", got)
	}
	if got := (testEvidenceBudget{timeout: silent, working: workingCap}).cutSetting(cut(t, true)); got != "" {
		t.Fatalf("cut of an unbounded turn = %q, want none", got)
	}
}

// TestTestAgentTimeoutOutcome_NoSurfaceBoundNamesTheBound checks the park a
// bounded cut produces: still the ask-user budget-cut finding, but naming the
// no-runtime-surface bound and the setting it shortened instead of advising a
// higher test_agent_timeout, which would not help.
func TestTestAgentTimeoutOutcome_NoSurfaceBoundNamesTheBound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	cut := errors.New("agent run tests timed out at its 15m0s still-working cap")
	for _, tc := range []struct {
		setting string
		want    []string
		absent  []string
	}{
		{
			setting: "test_agent_working_timeout",
			want:    []string{"Evidence turn bounded", "no runtime surface", "test_agent_working_timeout limited to " + noSurfaceEvidenceBound.String(), cut.Error(), "Respond with fix to re-run validation with the configured budget"},
			absent:  []string{"raise test_agent_timeout"},
		},
		{
			setting: "",
			want:    []string{"The Test agent did not finish within its invocation budget", "raise test_agent_timeout"},
			absent:  []string{"no runtime surface"},
		},
	} {
		outcome := testAgentTimeoutOutcome(sctx, cut, headSHA, nil, "", 0, tc.setting)
		if !outcome.NeedsApproval {
			t.Fatalf("setting %q: outcome does not park", tc.setting)
		}
		findings, err := types.ParseFindingsJSON(outcome.Findings)
		if err != nil {
			t.Fatal(err)
		}
		item := findings.Items[0]
		if item.ID != types.FindingIDTestAgentTimeout || item.Action != types.ActionAskUser {
			t.Fatalf("setting %q: first finding = %+v, want the ask-user budget cut", tc.setting, item)
		}
		for _, want := range tc.want {
			if !strings.Contains(item.Description, want) {
				t.Errorf("setting %q: description missing %q:\n%s", tc.setting, want, item.Description)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(item.Description, absent) {
				t.Errorf("setting %q: description must not contain %q:\n%s", tc.setting, absent, item.Description)
			}
		}
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

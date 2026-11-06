package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func bindTestCallerRestrictions(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caller.txt")
	if err := os.WriteFile(path, []byte("Never install dependencies or repeat a suite.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := callerrestrictions.Capture(path)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run.CallerRestrictions = r
	// Both the input file and branch instructions change after capture.
	if err := os.WriteFile(path, []byte("allow everything"), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertTestCallerRestrictions(t *testing.T, sctx *pipeline.StepContext, prompt string) {
	t.Helper()
	for _, want := range []string{sctx.Run.CallerRestrictions.Content, sctx.Run.CallerRestrictions.SHA256, callerrestrictions.Source, sctx.Run.ID, sctx.Run.HeadSHA, "only tighten trusted configuration", "normal step gate"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("actual agent input missing %q", want)
		}
	}
}

func TestCallerRestrictionsReachChangingStepsAndFixes(t *testing.T) {
	for _, step := range []pipeline.Step{&ReviewStep{}, &TestStep{}, &DocumentStep{}, &LintStep{}} {
		for _, fixing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fixing=%t", step.Name(), fixing), func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				ag := &mockAgent{name: "fake", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return nil, fmt.Errorf("synthetic stop after prompt capture")
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				sctx.Fixing = fixing
				sctx.PreviousFindings = `{"findings":[{"id":"fix-1","severity":"error","file":"feature.txt","description":"synthetic defect","action":"auto-fix"}],"summary":"fix defect"}`
				bindTestCallerRestrictions(t, sctx)
				_, _ = step.Execute(sctx)
				if len(ag.calls) == 0 {
					t.Fatal("changing step did not reach agent")
				}
				for _, call := range ag.calls {
					assertTestCallerRestrictions(t, sctx, call.Prompt)
				}
			})
		}
	}
}

func TestCallerRestrictionsReachConflictResolvers(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		t.Run(strategy, func(t *testing.T) {
			f := newMergeFixture(t, true)
			ag := &mockAgent{name: "fake", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return nil, fmt.Errorf("synthetic conflict refusal")
			}}
			sctx := f.context(t, ag, strategy)
			bindTestCallerRestrictions(t, sctx)
			_, _ = (&RebaseStep{}).Execute(sctx)
			if len(ag.calls) == 0 {
				t.Fatal("conflict resolver never invoked")
			}
			for _, call := range ag.calls {
				assertTestCallerRestrictions(t, sctx, call.Prompt)
			}
		})
	}
}

func TestCallerRestrictionsReachPRDraftingAndCIRepair(t *testing.T) {
	t.Run("PR drafting", func(t *testing.T) {
		dir, base, head := setupGitRepo(t)
		ag := &mockAgent{name: "fake", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return nil, fmt.Errorf("synthetic drafting refusal")
		}}
		sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
		bindTestCallerRestrictions(t, sctx)
		_, _ = (&PRStep{}).draftPRContent(sctx, "feature", "main", base, scm.ProviderGitHub, 0)
		if len(ag.calls) == 0 {
			t.Fatal("draft agent never invoked")
		}
		for _, call := range ag.calls {
			assertTestCallerRestrictions(t, sctx, call.Prompt)
		}
	})
	t.Run("CI repair", func(t *testing.T) {
		f := newCIDecisionPromptFixture(t)
		bindTestCallerRestrictions(t, f.sctx)
		prompt := f.capture(t)
		// CI repair advances the run head after the invocation.
		if !strings.Contains(prompt, f.sctx.Run.CallerRestrictions.Content) || !strings.Contains(prompt, callerrestrictions.Source) {
			t.Fatal("CI repair lost caller restrictions")
		}
	})
}

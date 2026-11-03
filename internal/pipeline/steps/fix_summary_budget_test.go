package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func budgetRule(n int) string {
	return fmt.Sprintf("at most %d characters", n)
}

func TestFixSummaryRules_BudgetFollowsRenderedPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		template string
		step     types.StepName
		want     int
	}{
		{"default review", "", types.StepReview, 51},
		{"default test", "", types.StepTest, 53},
		{"default lint", "", types.StepLint, 53},
		{"default document", "", types.StepDocument, 49},
		{"default custom gate", "", types.StepName("gate.test.mutation"), 72 - len("no-mistakes(gate.test.mutation): ")},
		{"custom template", "fix({{.Step}}): {{.Summary}}", types.StepReview, 59},
		{"summary only", "{{.Summary}}", types.StepReview, 72},
		{"text after summary", "{{.Summary}} [{{.Step}}]", types.StepReview, 72 - len(" [review]")},
		{"summary repeated", "{{.Summary}}{{.Summary}}", types.StepReview, 36},
		{"prefix longer than the limit hits the floor", strings.Repeat("p", 70) + "{{.Summary}}", types.StepReview, minFixSummaryBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sctx := &pipeline.StepContext{
				Run:    &db.Run{Branch: "refs/heads/feature"},
				Config: &config.Config{Commit: config.Commit{FixMessage: tt.template}},
			}
			got := fixSummaryRules(sctx, tt.step)
			if !strings.Contains(got, budgetRule(tt.want)) {
				t.Errorf("fixSummaryRules() = %q, want %s", got, budgetRule(tt.want))
			}
		})
	}
}

func TestFixSummaryRules_UnrenderableTemplateKeepsWordLimitOnly(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{
		Run:    &db.Run{Branch: "refs/heads/feature"},
		Config: &config.Config{Commit: config.Commit{FixMessage: "{{.Missing}}"}},
	}
	got := fixSummaryRules(sctx, types.StepReview)
	if strings.Contains(got, "characters") || !strings.Contains(got, "under 10 words") {
		t.Errorf("fixSummaryRules() = %q, want the word limit without a character budget", got)
	}
}

func TestFixPrompts_TellEachStepItsSummaryBudget(t *testing.T) {
	t.Parallel()
	gate := config.Gate{Name: "mutation-budget", After: types.StepTest, Command: "exit 0"}
	steps := []struct {
		name          string
		step          pipeline.Step
		cmds          config.Commands
		defaultBudget int
		customBudget  int
	}{
		{"review", &ReviewStep{}, config.Commands{}, 51, 59},
		{"test", &TestStep{}, config.Commands{Test: "exit 0"}, 53, 72 - len("fix(test): ")},
		{"lint", &LintStep{}, config.Commands{Lint: "exit 0"}, 53, 72 - len("fix(lint): ")},
		{"lint agent pass", &LintStep{}, config.Commands{}, 53, 72 - len("fix(lint): ")},
		{"document", &DocumentStep{}, config.Commands{}, 49, 72 - len("fix(document): ")},
		{"custom gate", &CustomGateStep{Gate: gate}, config.Commands{}, 72 - len("no-mistakes(gate.test.mutation-budget): "), 72 - len("fix(gate.test.mutation-budget): ")},
	}
	for _, tt := range steps {
		for _, template := range []string{"", "fix({{.Step}}): {{.Summary}}"} {
			wantBudget := tt.defaultBudget
			if template != "" {
				wantBudget = tt.customBudget
			}
			t.Run(fmt.Sprintf("%s/%q", tt.name, template), func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, headSHA := setupGitRepo(t)
				gitCmd(t, dir, "checkout", "--detach", headSHA)
				ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
						return nil, err
					}
					return &agent.Result{Output: json.RawMessage(`{"summary":"fix it","findings":[]}`)}, nil
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, tt.cmds)
				sctx.Fixing = true
				sctx.Config.Commit = config.Commit{FixMessage: template}
				sctx.PreviousFindings = `{"findings":[{"id":"f-1","severity":"warning","file":"main.go","description":"issue","action":"auto-fix"}],"items":[{"id":"f-1","severity":"warning","description":"issue"}],"summary":"1 issue"}`
				_, _ = tt.step.Execute(sctx)
				if len(ag.calls) == 0 {
					t.Fatal("agent was never called")
				}
				want := budgetRule(wantBudget)
				if prompt := ag.calls[0].Prompt; !strings.Contains(prompt, want) {
					t.Errorf("fix prompt missing %q:\n%s", want, prompt)
				}
			})
		}
	}
}

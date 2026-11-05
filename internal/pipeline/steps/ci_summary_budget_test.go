package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_FixPromptIncludesSummaryBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		template string
		budget   int
	}{
		{"default", "", 72 - len("no-mistakes(ci): ")},
		{"custom prefix", "fix(web): {{.Step}}: {{.Summary}}", 72 - len("fix(web): ci: ")},
		{"repeated summary", "fix: {{.Summary}} / {{.Summary}}", (72 - len("fix:  / ")) / 2},
		{"unicode prefix", "修复({{.Step}}): {{.Summary}}", 72 - utf8.RuneCountInString("修复(ci): ")},
	} {
		for _, mode := range []struct {
			name    string
			targets ciFixTargets
		}{
			{"failing check", ciTargetsFor([]string{"test"}, false)},
			{"check and conflict", ciTargetsFor([]string{"test"}, true)},
			{"conflict only", ciTargetsFor(nil, true)},
			{"behind base", ciFixTargets{BehindBase: true}},
			{"selected finding", ciFixTargetsFromFindings(Findings{Items: []Finding{{
				ID: "ci-1", Category: types.FindingCategoryCIReviewBot,
				Check: "Review", Description: "Repair the selected finding",
			}}})},
		} {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				dir, base, head := setupGitRepo(t)
				var prompt string
				ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					prompt = opts.Prompt
					return &agent.Result{Output: json.RawMessage(`{"summary":"external check failure","code_change_needed":false}`)}, nil
				}}
				sctx := newTestContext(t, ag, dir, base, head, config.Commands{})
				sctx.Config.Commit.FixMessage = tc.template
				pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
				repair, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, mode.targets)
				if err != nil {
					t.Fatal(err)
				}
				if repair.HeadAdvanced || gitCmd(t, dir, "rev-parse", "HEAD") != head {
					t.Fatal("no-change conclusion unexpectedly committed a repair")
				}
				if !mode.targets.MergeConflict && !mode.targets.BehindBase && (!repair.NoCodeChangeNeeded || repair.Summary != "external check failure") {
					t.Fatalf("no-code-change conclusion lost: %#v", repair)
				}
				if !strings.Contains(prompt, fixSummaryRules(sctx, types.StepCI)) || !strings.Contains(prompt, budgetRule(tc.budget)) {
					t.Errorf("CI fix prompt missing shared summary rules with budget %d:\n%s", tc.budget, prompt)
				}
				if !strings.Contains(prompt, "Keep detailed diagnosis and verification evidence in your explanatory output") {
					t.Error("CI fix prompt does not preserve diagnosis outside the summary")
				}
			})
		}
	}
}

func TestCIStep_ConciseRepairSummaryPreservesDiagnosticLog(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", head)
	const summary = "correct table count"
	diagnosis := strings.Repeat("The migration adds a table and the test expected the previous count. ", 20)
	var logged strings.Builder
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnChunk(diagnosis)
		if err := os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"` + summary + `","code_change_needed":true}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.CI.RevalidateRepairs = true
	sctx.Config.Commit.FixMessage = "fix(web): {{.Step}}: {{.Summary}}"
	sctx.LogChunk = func(chunk string) { logged.WriteString(chunk) }
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	repair, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"test"}, false))
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced || !repair.Revalidate || repair.Summary != summary {
		t.Fatalf("repair = %#v, want a locally recorded repair with concise summary", repair)
	}
	if got := gitCmd(t, dir, "log", "-1", "--format=%s"); got != "fix(web): ci: "+summary || utf8.RuneCountInString(got) > 72 {
		t.Fatalf("CI repair subject = %q", got)
	}
	if got := logged.String(); got != diagnosis {
		t.Fatalf("diagnosis log = %q, want original untruncated evidence", got)
	}
}

package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRoundHistory_ReviewFixIdentityAcrossPromptChannels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		step         types.StepName
		first        string
		last         string
		wantDeclined string
	}{
		{
			name: "moved unique review fix",
			step: types.StepReview,
			first: `{"findings":[{"id":"original","file":"a.go","line":10,"description":"missing check"}]}`,
			last: `{"findings":[{"id":"moved","file":"a.go","line":19,"description":"missing check"},{"id":"C","file":"c.go","description":"other defect"}]}`,
		},
		{
			name: "untouched review twin",
			step: types.StepReview,
			first: `{"findings":[{"id":"original","file":"a.go","line":10,"description":"missing check"},{"id":"twin","file":"a.go","line":30,"description":"missing check"}]}`,
			last: `{"findings":[{"id":"moved","file":"a.go","line":10,"description":"missing check"},{"id":"twin","file":"a.go","line":30,"description":"missing check"},{"id":"C","file":"c.go","description":"other defect"}]}`,
			wantDeclined: "twin,twin",
		},
		{
			name: "review fix reported at two new lines",
			step: types.StepReview,
			first: `{"findings":[{"id":"original","file":"a.go","line":10,"description":"missing check"}]}`,
			last: `{"findings":[{"id":"moved-a","file":"a.go","line":19,"description":"missing check"},{"id":"moved-b","file":"a.go","line":27,"description":"missing check"},{"id":"C","file":"c.go","description":"other defect"}]}`,
		},
		{
			name: "moved lint fix retains exact identity",
			step: types.StepLint,
			first: `{"findings":[{"id":"original","file":"a.go","line":10,"description":"missing check"}]}`,
			last: `{"findings":[{"id":"moved","file":"a.go","line":19,"description":"missing check"},{"id":"C","file":"c.go","description":"other defect"}]}`,
			wantDeclined: "moved",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionFixture(t)
			step := f.reviewSR
			if tc.step != types.StepReview {
				var err error
				step, err = f.db.InsertStepResult(f.run.ID, tc.step)
				if err != nil {
					t.Fatal(err)
				}
			}
			clean := `{"findings":[]}`
			for i, raw := range []string{tc.first, clean, tc.last} {
				r, err := f.db.InsertStepRound(step.ID, i+1, "auto_fix", &raw, nil, 0)
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 {
					continue
				}
				selection := `["original"]`
				if i == 2 {
					selection = `["C"]`
				}
				if err := f.db.SetStepRoundSelection(r.ID, &selection, db.RoundSelectionSourceUser); err != nil {
					t.Fatal(err)
				}
			}
			rounds, err := f.db.GetRoundsByStep(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			entries, _, err := f.db.GetBranchDecisionRounds(f.repo.ID, f.run.Branch, "next-run", db.MaxBranchDecisionRounds)
			if err != nil {
				t.Fatal(err)
			}
			own := &pipeline.StepContext{DB: f.db, Run: f.run, StepResultID: step.ID}
			branch := &pipeline.StepContext{DB: f.db, PriorBranchDecisions: entries}
			outputs := map[string]string{
				"own":        stepRoundHistorySection(own),
				"cross-step": runDecisionsPromptSection(f.testStepContext()),
				"branch":     branchDecisionsPromptSection(branch),
			}
			if tc.step == types.StepReview {
				outputs["uncertified"] = uncertifiedRoundHistoryPromptSection(&pipeline.StepContext{UncertifiedPriorRounds: rounds})
				outputs["superseded"] = supersededReviewHistoryPromptSection(&pipeline.StepContext{PreviousRunReviewRounds: rounds})
			}
			for channel, output := range outputs {
				t.Run(channel, func(t *testing.T) {
					if output == "" {
						t.Fatal("expected a generated decision history")
					}
					var declined []string
					inDeclines := false
					for _, line := range strings.Split(output, "\n") {
						if strings.HasPrefix(line, "user_") || strings.HasPrefix(line, "Round ") || line == "findings:" {
							inDeclines = line == "user_chose_to_ignore:"
						}
						if !inDeclines && !strings.Contains(line, " declined: ") {
							continue
						}
						start := strings.IndexByte(line, '{')
						if start < 0 {
							continue
						}
						var finding types.Finding
						if err := json.Unmarshal([]byte(line[start:]), &finding); err != nil {
							t.Fatal(err)
						}
						declined = append(declined, finding.ID)
					}
					if strings.Join(declined, ",") != tc.wantDeclined {
						t.Fatalf("declined = %v, want %q in generated history:\n%s", declined, tc.wantDeclined, output)
					}
				})
			}
		})
	}
}

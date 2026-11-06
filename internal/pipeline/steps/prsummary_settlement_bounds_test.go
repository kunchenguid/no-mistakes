package steps

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildPipelineSummary_SettlementCountsAreLowerBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		line int
	}{
		{"moved re-settlement", 19},
		{"distinct same-content instance", 30},
	} {
		for _, source := range []string{db.RoundSelectionSourceUserIgnored, db.RoundSelectionSourceExternal} {
			for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
				t.Run(fmt.Sprintf("%s/%s/%s", tc.name, source, provider), func(t *testing.T) {
					first := `{"findings":[{"id":"A","severity":"warning","file":"a.go","line":10,"description":"missing check"}]}`
					second := fmt.Sprintf(`{"findings":[{"id":"B","severity":"warning","file":"a.go","line":%d,"description":"missing check"}]}`, tc.line)
					empty := `{"findings":[]}`
					selection := db.DeclinedSelectionJSON
					rounds := []*db.StepRound{
						{Round: 1, Trigger: "initial", FindingsJSON: &first, SelectedFindingIDs: &selection, SelectionSource: &source},
						{Round: 2, Trigger: "external", FindingsJSON: &second, SelectedFindingIDs: &selection, SelectionSource: &source},
						{Round: 3, Trigger: "external", FindingsJSON: &empty},
					}
					if source == db.RoundSelectionSourceExternal {
						firstSettlement := `{"findings":[{"id":"A","severity":"warning","file":"a.go","line":10,"description":"missing check","user_instructions":"Synthetic verification for the first test settlement."}]}`
						secondSettlement := fmt.Sprintf(`{"findings":[{"id":"B","severity":"warning","file":"a.go","line":%d,"description":"missing check","user_instructions":"Synthetic verification for the second test settlement."}]}`, tc.line)
						rounds[0].UserFindingsJSON = &firstSettlement
						rounds[1].UserFindingsJSON = &secondSettlement
					}
					steps := []*db.StepResult{{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted}}
					md, _ := BuildPipelineSummaryFor(steps, map[string][]*db.StepRound{"review": rounds}, testPipelineHeadSHA, provider)
					if !strings.Contains(md, "→ at least 1 settled by the operator without a fix ✅") {
						t.Fatalf("step summary must report the provable lower bound:\n%s", md)
					}
					if !strings.Contains(md, "✅ No issues remain - at least 1 settled by the operator without a fix.") {
						t.Fatalf("round detail must report the provable lower bound:\n%s", md)
					}
				})
			}
		}
	}
}

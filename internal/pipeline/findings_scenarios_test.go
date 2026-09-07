package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A gate response that selects, excludes, or merges findings rewrites the
// recorded findings row. The Test step's evidence - its artifacts, its
// scenario table, and its verdict - is not a finding and must survive that
// rewrite intact, or answering a gate would quietly erase the record of what
// the step actually drove.
func TestFindingsSelectionKeepsTestEvidence(t *testing.T) {
	t.Parallel()
	original, err := types.MarshalFindingsJSON(types.Findings{
		Items: []types.Finding{
			{ID: "t-1", Severity: "error", Description: "checkout failed", Action: types.ActionAutoFix},
			{ID: "t-2", Severity: "warning", Description: "slow first paint", Action: types.ActionAskUser},
		},
		Summary:        "one failure",
		Tested:         []string{"`npm run e2e -- checkout`"},
		TestingSummary: "drove checkout against a running app",
		Artifacts:      []types.TestArtifact{{Kind: "screenshot", Label: "checkout", Path: "checkout.png"}},
		Scenarios:      []types.TestScenario{{Name: "user reaches the success screen", Result: types.ScenarioResultFail, Live: true}},
		Verdict:        types.TestVerdictNoGo,
	})
	if err != nil {
		t.Fatal(err)
	}

	keepOne, err := types.MarshalFindingsJSON(types.Findings{
		Items: []types.Finding{{ID: "t-1", Severity: "error", Description: "checkout failed", Action: types.ActionAutoFix}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, got := range map[string]string{
		"select by id":     filterFindingsJSON(original, []string{"t-1"}),
		"select none":      filterFindingsJSON(original, nil),
		"retain matching":  retainMatchingFindingsJSON(original, keepOne),
		"remove matching":  removeMatchingFindingsJSON(original, keepOne),
		"merge additional": mergeFindingsJSON(original, keepOne),
	} {
		parsed, err := types.ParseFindingsJSON(got)
		if err != nil {
			t.Fatalf("%s: %v (payload %s)", name, err, got)
		}
		if len(parsed.Scenarios) != 1 || parsed.Scenarios[0].Name != "user reaches the success screen" {
			t.Errorf("%s dropped the scenario record: %s", name, got)
		}
		if parsed.Verdict != types.TestVerdictNoGo {
			t.Errorf("%s dropped the verdict: %s", name, got)
		}
		if len(parsed.Artifacts) != 1 {
			t.Errorf("%s dropped the evidence artifacts: %s", name, got)
		}
		if len(parsed.Tested) != 1 || !strings.Contains(parsed.TestingSummary, "checkout") {
			t.Errorf("%s dropped the tested record: %s", name, got)
		}
	}
}

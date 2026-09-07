package types

import "testing"

// The CI step routes its fix rounds by Finding.Check and Finding.Category, and
// both travel through the same JSON the executor persists and hands back as
// PreviousFindings. Finding.UnmarshalJSON copies fields explicitly, so a
// field it does not name is silently dropped on every parse.
func TestParseFindingsJSON_RoundTripsCICheckFields(t *testing.T) {
	t.Parallel()
	encoded, err := MarshalFindingsJSON(Findings{
		Summary: "1 CI check failing",
		Items: []Finding{{
			ID:          "ci-1",
			Severity:    FindingSeverityError,
			Action:      ActionAutoFix,
			Category:    FindingCategoryCICheck,
			Check:       "test (ubuntu-latest)",
			CheckID:     "github-check-run:42",
			Description: "CI check failing: test (ubuntu-latest)",
		}},
	})
	if err != nil {
		t.Fatalf("MarshalFindingsJSON() error = %v", err)
	}
	parsed, err := ParseFindingsJSON(encoded)
	if err != nil {
		t.Fatalf("ParseFindingsJSON() error = %v", err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("items = %+v, want one", parsed.Items)
	}
	item := parsed.Items[0]
	if item.Check != "test (ubuntu-latest)" || item.CheckID != "github-check-run:42" || item.Category != FindingCategoryCICheck {
		t.Fatalf("item = %+v, want the check name and category preserved", item)
	}

	legacy, err := ParseFindingsJSON(`{"findings":[{"severity":"warning","description":"CI check failing: test"}],"summary":"x"}`)
	if err != nil {
		t.Fatalf("ParseFindingsJSON(legacy) error = %v", err)
	}
	if legacy.Items[0].Check != "" || legacy.Items[0].CheckID != "" || legacy.Items[0].Category != "" {
		t.Fatalf("legacy item = %+v, want empty CI fields", legacy.Items[0])
	}
	if legacy.Items[0].ActionOrDefault() != ActionAskUser {
		t.Fatalf("legacy action = %q, want the ask-user default", legacy.Items[0].ActionOrDefault())
	}
}

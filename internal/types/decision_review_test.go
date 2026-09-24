package types

import (
	"reflect"
	"testing"
)

func TestDecisionReviewsSurviveFindingsNormalization(t *testing.T) {
	want := []DecisionReview{{DecisionID: "round/finding", Result: "contradicted", Evidence: "source no longer implements the chosen behavior"}}
	original := Findings{DecisionReviews: want, Items: []Finding{{Severity: "warning", Description: "recorded decision reversed", Action: ActionAskUser}}}
	raw, err := MarshalFindingsJSON(original)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := NormalizeFindings(parsed, "review")
	if !reflect.DeepEqual(got.DecisionReviews, want) {
		t.Fatalf("lost decision evidence: %+v", got)
	}
}

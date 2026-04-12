package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMergeFindingsJSON_KeepsDistinctFindingsWithSameAutoID(t *testing.T) {
	existingRaw := `{"findings":[{"id":"review-1","severity":"warning","description":"first"}],"summary":"1 finding"}`
	additionalRaw := `{"findings":[{"id":"review-1","severity":"error","description":"second"}],"summary":"1 finding"}`

	mergedRaw := mergeFindingsJSON(existingRaw, additionalRaw)
	merged, err := types.ParseFindingsJSON(mergedRaw)
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(merged.Items) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(merged.Items))
	}
	if merged.Items[0].Description != "first" || merged.Items[1].Description != "second" {
		t.Fatalf("unexpected merged findings: %#v", merged.Items)
	}
}

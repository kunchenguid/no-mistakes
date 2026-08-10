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

func TestMergeFindingsJSON_UnionsTestEvidenceFromBothSides(t *testing.T) {
	existingRaw := `{"findings":[{"id":"test-1","severity":"warning","description":"fresh"}],"summary":"1 finding","tested":["login flow"],"testing_summary":"re-ran the fixed check","artifacts":[{"kind":"log","label":"fix round log","path":"/tmp/fix.log"}]}`
	additionalRaw := `{"findings":[{"id":"test-2","severity":"error","description":"carried"}],"summary":"1 finding","tested":["login flow","signup flow"],"testing_summary":"ran the suite","artifacts":[{"kind":"log","label":"first round log","path":"/tmp/first.log"}]}`

	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(existingRaw, additionalRaw))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	labels := make([]string, 0, len(merged.Artifacts))
	for _, artifact := range merged.Artifacts {
		labels = append(labels, artifact.Label)
	}
	if len(merged.Artifacts) != 2 || labels[0] != "fix round log" || labels[1] != "first round log" {
		t.Fatalf("expected both rounds' artifacts, fresh first, got %#v", merged.Artifacts)
	}
	if len(merged.Tested) != 2 || merged.Tested[0] != "login flow" || merged.Tested[1] != "signup flow" {
		t.Fatalf("expected the union of tested entries with no duplicates, got %#v", merged.Tested)
	}
	if merged.TestingSummary != "re-ran the fixed check" {
		t.Fatalf("testing_summary should stay the fresh round's assessment, got %q", merged.TestingSummary)
	}
}

func TestMergeFindingsJSON_KeepsCarriedEvidenceWhenFreshRoundReportsNone(t *testing.T) {
	existingRaw := `{"findings":[{"id":"test-1","severity":"warning","description":"fresh"}],"summary":"1 finding","testing_summary":"re-ran the fixed check"}`
	additionalRaw := `{"findings":[{"id":"test-2","severity":"error","description":"carried"}],"summary":"1 finding","tested":["login flow"],"artifacts":[{"kind":"log","label":"first round log","path":"/tmp/first.log"}]}`

	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(existingRaw, additionalRaw))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(merged.Artifacts) != 1 || merged.Artifacts[0].Label != "first round log" {
		t.Fatalf("expected the carried artifact to survive a fresh round that produced none, got %#v", merged.Artifacts)
	}
	if len(merged.Tested) != 1 || merged.Tested[0] != "login flow" {
		t.Fatalf("expected the carried tested entries to survive, got %#v", merged.Tested)
	}
}

func TestRetainMatchingFindingsJSON_DropsFindingsMissingFromLatestReview(t *testing.T) {
	existingRaw := `{"findings":[{"id":"review-1","severity":"warning","description":"first"},{"id":"review-2","severity":"error","description":"second"}],"summary":"2 findings"}`
	keepRaw := `{"findings":[{"id":"review-7","severity":"error","description":"second"},{"id":"review-8","severity":"warning","description":"third"}],"summary":"2 findings"}`

	retainedRaw := retainMatchingFindingsJSON(existingRaw, keepRaw)
	retained, err := types.ParseFindingsJSON(retainedRaw)
	if err != nil {
		t.Fatalf("parse retained findings: %v", err)
	}
	if len(retained.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(retained.Items))
	}
	if retained.Items[0].Description != "second" {
		t.Fatalf("unexpected retained findings: %#v", retained.Items)
	}
}

func TestRetainMatchingFindingsJSON_MatchesFindingsAfterLineShift(t *testing.T) {
	existingRaw := `{"findings":[{"id":"dismissed-1","severity":"warning","file":"internal/pipeline/findings.go","line":42,"description":"still unresolved"}],"summary":"1 finding"}`
	keepRaw := `{"findings":[{"id":"review-9","severity":"warning","file":"internal/pipeline/findings.go","line":57,"description":"still unresolved"}],"summary":"1 finding"}`

	retainedRaw := retainMatchingFindingsJSON(existingRaw, keepRaw)
	retained, err := types.ParseFindingsJSON(retainedRaw)
	if err != nil {
		t.Fatalf("parse retained findings: %v", err)
	}
	if len(retained.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(retained.Items))
	}
	if retained.Items[0].ID != "dismissed-1" {
		t.Fatalf("unexpected retained finding: %#v", retained.Items)
	}
}

func TestRetainMatchingFindingsJSON_DoesNotKeepDistinctDuplicateLines(t *testing.T) {
	existingRaw := `{"findings":[{"id":"dismissed-1","severity":"warning","file":"internal/pipeline/findings.go","line":42,"description":"still unresolved"},{"id":"dismissed-2","severity":"warning","file":"internal/pipeline/findings.go","line":57,"description":"still unresolved"}],"summary":"2 findings"}`
	keepRaw := `{"findings":[{"id":"review-9","severity":"warning","file":"internal/pipeline/findings.go","line":42,"description":"still unresolved"}],"summary":"1 finding"}`

	retainedRaw := retainMatchingFindingsJSON(existingRaw, keepRaw)
	retained, err := types.ParseFindingsJSON(retainedRaw)
	if err != nil {
		t.Fatalf("parse retained findings: %v", err)
	}
	if len(retained.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(retained.Items))
	}
	if retained.Items[0].ID != "dismissed-1" {
		t.Fatalf("unexpected retained findings: %#v", retained.Items)
	}
}

func TestAutoFixableFindingsJSON_FiltersToAutoFix(t *testing.T) {
	raw := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"design choice","action":"ask-user"},{"id":"review-3","severity":"warning","description":"missing check","action":"auto-fix"},{"id":"review-4","severity":"info","description":"note","action":"no-op"}],"risk_level":"medium","risk_rationale":"Mixed."}`

	fixableRaw := autoFixableFindingsJSON(raw)
	fixable, err := types.ParseFindingsJSON(fixableRaw)
	if err != nil {
		t.Fatalf("parse auto-fixable findings: %v", err)
	}
	if len(fixable.Items) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(fixable.Items))
	}
	if fixable.Items[0].ID != "review-1" || fixable.Items[1].ID != "review-3" {
		t.Fatalf("unexpected findings: %#v", fixable.Items)
	}
}

func TestAutoFixableFindingsJSON_AllAskUser(t *testing.T) {
	raw := `{"findings":[{"id":"review-1","severity":"warning","description":"choice","action":"ask-user"}],"risk_level":"high","risk_rationale":"Needs review."}`

	fixableRaw := autoFixableFindingsJSON(raw)
	if fixableRaw != "" {
		t.Fatalf("expected empty string for all-ask-user findings, got %q", fixableRaw)
	}
}

func TestAutoFixableFindingsJSON_EmptyInput(t *testing.T) {
	if got := autoFixableFindingsJSON(""); got != "" {
		t.Fatalf("expected empty string for empty input, got %q", got)
	}
}

func TestAutoFixableFindingsJSON_AllNoOp(t *testing.T) {
	raw := `{"findings":[{"id":"review-1","severity":"info","description":"note","action":"no-op"}],"risk_level":"low","risk_rationale":"Clean."}`

	fixableRaw := autoFixableFindingsJSON(raw)
	if fixableRaw != "" {
		t.Fatalf("expected empty string for all-no-op findings, got %q", fixableRaw)
	}
}

func TestMergeFindingsJSON_DeduplicatesShiftedUniqueDismissedFinding(t *testing.T) {
	existingRaw := `{"findings":[{"id":"dismissed-1","severity":"warning","file":"internal/pipeline/findings.go","line":42,"description":"still unresolved"}],"summary":"1 finding"}`
	additionalRaw := `{"findings":[{"id":"dismissed-2","severity":"warning","file":"internal/pipeline/findings.go","line":57,"description":"still unresolved"}],"summary":"1 finding"}`

	mergedRaw := mergeFindingsJSON(existingRaw, additionalRaw)
	merged, err := types.ParseFindingsJSON(mergedRaw)
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(merged.Items))
	}
	if merged.Items[0].ID != "dismissed-1" {
		t.Fatalf("unexpected merged findings: %#v", merged.Items)
	}
}

func TestFilterFindingsJSON_EmptySelectionReturnsEmptyFindings(t *testing.T) {
	raw := `{"findings":[{"id":"review-1","severity":"error","description":"first"}],"summary":"1 finding"}`

	filteredRaw := filterFindingsJSON(raw, nil)
	filtered, err := types.ParseFindingsJSON(filteredRaw)
	if err != nil {
		t.Fatalf("parse filtered findings: %v", err)
	}
	if len(filtered.Items) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(filtered.Items))
	}
	if filtered.Summary != "0 selected findings" {
		t.Fatalf("summary = %q, want %q", filtered.Summary, "0 selected findings")
	}
}

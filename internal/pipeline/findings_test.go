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

// Different descriptions do not fingerprint-collide; cover both the issue's
// sketch and the equal-description case that actually exercises deduplication.
func TestMergeFindingsJSON_UserAdditionStaysSelected(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string
		line        int
		wantCount   int
		action      string
		source      string
	}{
		{"different description", "please fix the note", 10, 2, types.ActionNoOp, ""},
		{"exact collision", "note", 10, 1, types.ActionNoOp, ""},
		{"shifted collision", "note", 11, 1, types.ActionNoOp, ""},
		{"blocking collision", "note", 10, 2, types.ActionAutoFix, ""},
		{"independent user collision", "note", 10, 2, types.ActionAutoFix, types.FindingSourceUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := types.Findings{Items: []types.Finding{{ID: "review-1", Severity: "warning", File: "file.go", Line: 10, Description: "note", Action: tc.action, Source: tc.source}}}
			added := types.Finding{ID: "user-1", Severity: "warning", File: "file.go", Line: tc.line, Description: tc.description, Action: types.ActionAutoFix, Source: types.FindingSourceUser, UserInstructions: "preserve this instruction"}
			existingRaw, err := types.MarshalFindingsJSON(existing)
			if err != nil {
				t.Fatal(err)
			}
			additionalRaw, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{added}})
			if err != nil {
				t.Fatal(err)
			}
			mergedRaw := mergeFindingsJSON(existingRaw, additionalRaw)
			_, _, outstanding, persisted := normalizeFixSelection(existingRaw, approvalResponse{addedFindings: []types.Finding{added}}, true)
			for name, raw := range map[string]string{"outstanding": outstanding, "persisted": persisted} {
				selected, err := types.ParseFindingsJSON(filterFindingsJSON(raw, []string{added.ID}))
				if err != nil {
					t.Fatal(err)
				}
				if len(selected.Items) != 1 || selected.Items[0] != added {
					t.Fatalf("%s selection = %#v, want %#v", name, selected.Items, added)
				}
			}
			merged, err := types.ParseFindingsJSON(mergedRaw)
			if err != nil {
				t.Fatal(err)
			}
			if len(merged.Items) != tc.wantCount {
				t.Fatalf("merged = %#v, want %d findings", merged.Items, tc.wantCount)
			}
			selected, err := types.ParseFindingsJSON(filterFindingsJSON(mergedRaw, []string{added.ID}))
			if err != nil {
				t.Fatal(err)
			}
			if len(selected.Items) != 1 || selected.Items[0] != added {
				t.Fatalf("selected = %#v, want user addition %#v", selected.Items, added)
			}
		})
	}
}

func TestRemapFindingIDsJSON_ReservesExactIDsBeforeContentMatches(t *testing.T) {
	gate := types.Findings{Items: []types.Finding{
		{ID: "review-1", Severity: "warning", File: "file.go", Line: 10, Description: "same content", Action: types.ActionNoOp},
		{ID: "review-2", Severity: "warning", File: "file.go", Line: 10, Description: "same content", Action: types.ActionAutoFix},
	}}
	added := types.Finding{
		ID:               "user-1",
		Severity:         "warning",
		File:             "file.go",
		Line:             10,
		Description:      "same content",
		Action:           types.ActionAutoFix,
		Source:           types.FindingSourceUser,
		UserInstructions: "keep the user-authored repair",
	}
	gateRaw, err := types.MarshalFindingsJSON(gate)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, persistedRaw := normalizeFixSelection(gateRaw, approvalResponse{
		findingIDs:    []string{"review-2"},
		addedFindings: []types.Finding{added},
	}, true)
	persisted, err := types.ParseFindingsJSON(persistedRaw)
	if err != nil {
		t.Fatalf("parse persisted selection: %v", err)
	}
	byID := make(map[string]types.Finding, len(persisted.Items))
	for _, item := range persisted.Items {
		byID[item.ID] = item
	}
	if got := byID["review-2"]; got.Action != types.ActionAutoFix || got.Source == types.FindingSourceUser {
		t.Fatalf("blocking finding recovered under the wrong identity: %#v", got)
	}
	if got := byID["user-1"]; got != added {
		t.Fatalf("user finding recovered under the wrong identity: %#v, want %#v", got, added)
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

func TestMergeFindingsJSON_LatestRiskAssessmentWinsWhenFindingsCarryForward(t *testing.T) {
	existingRaw := `{"findings":[{"id":"review-1","severity":"warning","description":"first"}],"summary":"1 finding","risk_level":"medium","risk_rationale":"first and second should be addressed","risk_scope":"scope-a"}`
	additionalRaw := `{"findings":[{"id":"review-9","severity":"info","description":"other"}],"summary":"1 finding","risk_level":"low","risk_rationale":"first and second are fixed","risk_scope":"scope-b"}`

	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(existingRaw, additionalRaw))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(merged.Items) != 2 {
		t.Fatalf("expected carried finding plus new finding, got %#v", merged.Items)
	}
	if merged.RiskLevel != "low" || merged.RiskRationale != "first and second are fixed" || merged.RiskScope != "scope-b" {
		t.Fatalf("expected latest assessment, got %q / %q / %q", merged.RiskLevel, merged.RiskRationale, merged.RiskScope)
	}
}

func TestMergeFindingsJSON_KeepsExistingRiskAssessmentWhenRoundHasNone(t *testing.T) {
	existingRaw := `{"findings":[{"id":"review-1","severity":"warning","description":"first"}],"summary":"1 finding","risk_level":"medium","risk_rationale":"first should be addressed","risk_scope":"scope-a"}`
	additionalRaw := `{"findings":[{"id":"review-9","severity":"info","description":"other"}],"summary":"1 finding"}`

	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(existingRaw, additionalRaw))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if merged.RiskLevel != "medium" || merged.RiskRationale != "first should be addressed" || merged.RiskScope != "scope-a" {
		t.Fatalf("expected existing assessment kept, got %q / %q / %q", merged.RiskLevel, merged.RiskRationale, merged.RiskScope)
	}
}

func TestMergeOutstandingFindingsJSON_LatestRiskAssessmentWins(t *testing.T) {
	existingRaw := `{"findings":[{"id":"review-1","severity":"warning","description":"first","file":"a.go"}],"summary":"1 finding","risk_level":"medium","risk_rationale":"first should be addressed"}`
	roundRaw := `{"findings":[{"id":"review-1","severity":"warning","description":"first","file":"a.go"}],"summary":"1 finding","risk_level":"low","risk_rationale":"only first remains"}`

	merged, err := types.ParseFindingsJSON(mergeOutstandingFindingsJSON(existingRaw, roundRaw, []string{"a.go"}))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if merged.RiskLevel != "low" || merged.RiskRationale != "only first remains" {
		t.Fatalf("expected latest assessment, got %q / %q", merged.RiskLevel, merged.RiskRationale)
	}
}

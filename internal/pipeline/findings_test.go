package pipeline

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func marshalFindingItems(t *testing.T, items ...types.Finding) string {
	t.Helper()
	raw, err := types.MarshalFindingsJSON(types.Findings{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parseFindingItems(t *testing.T, raw string) []types.Finding {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Items
}

func TestSupportedClaimsStayDistinctAcrossReviewCarryAndSelection(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := types.Finding{ID: "review-1", Severity: types.FindingSeverityWarning, File: "feature.go", Line: 8,
		Description: "historical check needs proof", Action: types.ActionNoOp, Category: types.FindingCategoryReviewSupportPending}
	first := base
	first.Support = &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "build", HeadSHA: head}}
	second := base
	second.Support = &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "security", HeadSHA: head}}
	merged := mergeOutstandingFindingsJSON(marshalFindingItems(t, first), marshalFindingItems(t, second), nil)
	items := parseFindingItems(t, merged)
	if len(items) != 2 || items[0].Support.CI.CheckID != "build" || items[1].Support.CI.CheckID != "security" || items[0].ID == items[1].ID {
		t.Fatalf("distinct CI claims were not carried independently: %+v", items)
	}
	if types.ReviewSupportClaimID(items[0]) == types.ReviewSupportClaimID(items[1]) {
		t.Fatal("distinct named checks share an owner claim ID")
	}
	selected := parseFindingItems(t, remapFindingIDsJSON(merged, marshalFindingItems(t, second)))
	if len(selected) != 1 || selected[0].ID != items[1].ID {
		t.Fatalf("selection remapped to another claim: %+v", selected)
	}
	remaining := parseFindingItems(t, removeMatchingFindingsJSON(merged, marshalFindingItems(t, second)))
	if len(remaining) != 1 || remaining[0].Support.CI.CheckID != "build" {
		t.Fatalf("removal erased an unrelated claim: %+v", remaining)
	}
	retained := parseFindingItems(t, retainMatchingFindingsJSON(merged, marshalFindingItems(t, second)))
	if len(retained) != 1 || retained[0].Support.CI.CheckID != "security" {
		t.Fatalf("retention selected an unrelated claim: %+v", retained)
	}
	if got := retainFindingIDsByIdentity(marshalFindingItems(t, second), []string{first.ID}, map[string]types.Finding{first.ID: first}); len(got) != 0 {
		t.Fatalf("recovery reused a selected ID for another claim: %v", got)
	}
}

func TestSupportedFindingSelectorsAndFreshEvidence(t *testing.T) {
	base := types.Finding{ID: "review-1", Severity: types.FindingSeverityWarning, File: "feature.go", Line: 8,
		Description: "claim needs proof", Action: types.ActionNoOp}
	for _, tc := range []struct {
		name     string
		a, b     *types.FindingSupport
		wantSame bool
	}{
		{"CI historical head", &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "build", HeadSHA: "old"}}, &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "build", HeadSHA: "new"}}, false},
		{"Test command", &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./a"}}, &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./b"}}, false},
		{"claim type", &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "build"}}, &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "build", HeadSHA: "old"}}, false},
		{"source path", &types.FindingSupport{ClaimType: types.FindingClaimSource, Source: &types.FindingSourceSupport{Path: "feature.go", Line: 8, Quote: "same"}}, &types.FindingSupport{ClaimType: types.FindingClaimSource, Source: &types.FindingSourceSupport{Path: "other.go", Line: 8, Quote: "same"}}, false},
		{"runtime transition", &types.FindingSupport{ClaimType: types.FindingClaimRuntime, Runtime: &types.FindingRuntimeSupport{Entity: "service", Transition: "down", TransitionAt: "earlier", Revision: "a"}}, &types.FindingSupport{ClaimType: types.FindingClaimRuntime, Runtime: &types.FindingRuntimeSupport{Entity: "service", Transition: "down", TransitionAt: "later", Revision: "a"}}, false},
		{"runtime revision", &types.FindingSupport{ClaimType: types.FindingClaimRuntime, Runtime: &types.FindingRuntimeSupport{Entity: "service", Transition: "down", TransitionAt: "earlier", Revision: "a"}}, &types.FindingSupport{ClaimType: types.FindingClaimRuntime, Runtime: &types.FindingRuntimeSupport{Entity: "service", Transition: "down", TransitionAt: "earlier", Revision: "b"}}, false},
		{"source quote refresh", &types.FindingSupport{ClaimType: types.FindingClaimSource, Source: &types.FindingSourceSupport{Path: "feature.go", Line: 8, Quote: "old"}}, &types.FindingSupport{ClaimType: types.FindingClaimSource, Source: &types.FindingSourceSupport{Path: "feature.go", Line: 9, Quote: "new", HeadSHA: "fresh"}}, true},
		{"owner result refresh", &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "build", HeadSHA: "old"}}, &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "build", HeadSHA: "old"}, OwnerResult: &types.FindingOwnerResult{CheckState: "pass"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := base, base
			a.Support, b.Support = tc.a, tc.b
			items := parseFindingItems(t, mergeFindingsJSON(marshalFindingItems(t, a), marshalFindingItems(t, b)))
			want := 2
			if tc.wantSame {
				want = 1
			}
			if len(items) != want {
				t.Fatalf("merged %d claims, want %d: %+v", len(items), want, items)
			}
			if tc.wantSame && !reflect.DeepEqual(items[0].Support, tc.b) {
				t.Fatalf("new evidence was not retained: %+v", items[0].Support)
			}
		})
	}
}

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

func TestMergeFindingsJSON_DeduplicatesSupportedClaimAndRefreshesProof(t *testing.T) {
	existing := `{"findings":[{"id":"finding-1","severity":"warning","file":"feature.txt","line":1,"description":"unsafe value","action":"ask-user","support":{"claim_type":"source","source":{"path":"feature.txt","line":1,"quote":"old"}}}]}`
	current := `{"findings":[{"id":"finding-1","severity":"warning","file":"feature.txt","line":1,"description":"unsafe value","action":"ask-user","support":{"claim_type":"source","source":{"path":"feature.txt","line":1,"quote":"current"}}}]}`
	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(existing, current))
	if err != nil || len(merged.Items) != 1 {
		t.Fatalf("supported claim duplicated: %+v, %v", merged, err)
	}
	if got := merged.Items[0].Support.Source.Quote; got != "current" {
		t.Fatalf("supported claim kept stale proof %q", got)
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

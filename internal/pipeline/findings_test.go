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

func TestMergeFindingsJSON_RestatementCannotDowngradeACarriedAction(t *testing.T) {
	// The fresh round restates the carried finding's content verbatim but
	// calls it auto-fix. Only a respond action can change what the operator
	// was shown, so the carried ask-user must survive - otherwise the step
	// completes with an unresolved blocker silently reclassified.
	freshRaw := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","description":"copy is dishonest","action":"auto-fix"}],"summary":"1 finding"}`
	carriedRaw := `{"findings":[{"id":"review-3","severity":"error","file":"a.go","description":"copy is dishonest","action":"ask-user"}],"summary":"1 outstanding finding"}`

	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(freshRaw, carriedRaw))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected the restatement to collapse onto the carried finding, got %d items: %#v", len(merged.Items), merged.Items)
	}
	if merged.Items[0].Action != types.ActionAskUser {
		t.Errorf("action = %q, want ask-user: a round's own restatement must not downgrade a carried finding", merged.Items[0].Action)
	}
	if merged.Items[0].ID != "review-3" {
		t.Errorf("id = %q, want review-3: the operator was shown that id and selects by it", merged.Items[0].ID)
	}
	if !types.HasAskUserFindings(merged) {
		t.Error("the merged payload must still block the step")
	}
}

func TestMergeFindingsJSON_UnionSummaryAndRiskDoNotUnderstateCarriedFindings(t *testing.T) {
	freshRaw := `{"findings":[{"id":"review-1","severity":"info","description":"a nit"}],"summary":"1 finding","risk_level":"low","risk_rationale":"just a nit"}`
	carriedRaw := `{"findings":[
		{"id":"review-7","severity":"error","description":"first blocker","action":"ask-user"},
		{"id":"review-8","severity":"error","description":"second blocker","action":"ask-user"},
		{"id":"review-9","severity":"error","description":"third blocker","action":"ask-user"}
	],"summary":"3 outstanding findings","risk_level":"high","risk_rationale":"three unresolved blockers"}`

	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(freshRaw, carriedRaw))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(merged.Items) != 4 {
		t.Fatalf("expected the union of 1 fresh and 3 carried findings, got %d", len(merged.Items))
	}
	count, ok := leadingCount(merged.Summary)
	if !ok || count != len(merged.Items) {
		t.Errorf("summary %q must count the %d findings the gate actually shows", merged.Summary, len(merged.Items))
	}
	if merged.RiskLevel != "high" {
		t.Errorf("risk_level = %q, want high: a low-risk fresh round must not present three carried blockers as low risk", merged.RiskLevel)
	}
	if merged.RiskRationale != "three unresolved blockers" {
		t.Errorf("risk_rationale = %q, want the assessment that justifies the presented level", merged.RiskRationale)
	}
}

func TestMergeFindingsJSON_KeepsFreshAssessmentWhenNothingIsCarried(t *testing.T) {
	freshRaw := `{"findings":[{"id":"review-1","severity":"info","description":"a nit"}],"summary":"1 finding","risk_level":"low","risk_rationale":"just a nit"}`
	carriedRaw := `{"findings":[{"id":"review-7","severity":"info","description":"a nit"}],"summary":"1 outstanding finding","risk_level":"high","risk_rationale":"stale"}`

	merged, err := types.ParseFindingsJSON(mergeFindingsJSON(freshRaw, carriedRaw))
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected the duplicate to collapse, got %d", len(merged.Items))
	}
	if merged.RiskLevel != "high" {
		t.Errorf("risk_level = %q: the carried finding is still outstanding, so its level still applies", merged.RiskLevel)
	}
}

func TestDedupeFindingIDsJSON_RenamesTheFreshItemNotTheCarriedOne(t *testing.T) {
	carriedRaw := `{"findings":[{"id":"review-1","severity":"error","description":"old carried issue","action":"ask-user"}],"summary":"1 outstanding finding"}`
	// Both items claim review-1: the fresh one because this round normalized
	// against its own single-item list.
	mergedRaw := `{"findings":[
		{"id":"review-1","severity":"warning","description":"brand new issue","action":"ask-user"},
		{"id":"review-1","severity":"error","description":"old carried issue","action":"ask-user"}
	],"summary":"2 outstanding findings"}`

	deduped, err := types.ParseFindingsJSON(dedupeFindingIDsJSON(mergedRaw, "review", carriedRaw))
	if err != nil {
		t.Fatalf("parse deduped findings: %v", err)
	}
	byDescription := map[string]string{}
	for _, item := range deduped.Items {
		if prior, ok := byDescription[item.Description]; ok {
			t.Fatalf("unexpected duplicate description %q (ids %q and %q)", item.Description, prior, item.ID)
		}
		byDescription[item.Description] = item.ID
	}
	if byDescription["old carried issue"] != "review-1" {
		t.Errorf("the already-shown carried finding lost its id (now %q); an axi respond --findings review-1 composed at the last gate would now select something else", byDescription["old carried issue"])
	}
	if byDescription["brand new issue"] == "review-1" || byDescription["brand new issue"] == "" {
		t.Errorf("the newly reported finding should have been renamed, got %q", byDescription["brand new issue"])
	}
}

func TestSanitizeFabricatedApprovalJSON_CoversAnyClaimedHumanApprover(t *testing.T) {
	fabricated := []string{
		"the remaining items were explicitly accepted by the user",
		"the maintainer approved these items",
		"the operator signed off on the remaining findings",
		"approved by the reviewer",
		"the human confirmed this is acceptable",
		"these findings have been approved",
		"the owner already okayed the remaining work",
		"authorized by our team",
	}
	for _, rationale := range fabricated {
		raw := `{"findings":[{"id":"review-1","severity":"error","description":"blocker","action":"ask-user"}],"summary":"1 finding","risk_level":"low","risk_rationale":"` + rationale + `"}`
		sanitized, stripped := sanitizeFabricatedApprovalJSON(raw)
		if !stripped {
			t.Errorf("rationale %q claims a human approval this run cannot corroborate and must be stripped", rationale)
			continue
		}
		parsed, err := types.ParseFindingsJSON(sanitized)
		if err != nil {
			t.Fatalf("parse sanitized findings: %v", err)
		}
		if parsed.RiskRationale == rationale {
			t.Errorf("rationale %q survived verbatim", rationale)
		}
	}
}

func TestSanitizeFabricatedApprovalJSON_LeavesOrdinaryTechnicalProseAlone(t *testing.T) {
	honest := []string{
		"the failures were confirmed by re-running the targeted checks",
		"the change is well-bounded and the remaining findings are unresolved",
		"both maintainers of this package are listed in CODEOWNERS",
		"the parties agreed on nothing; these findings remain open",
	}
	for _, rationale := range honest {
		raw := `{"findings":[{"id":"review-1","severity":"error","description":"blocker","action":"ask-user"}],"summary":"1 finding","risk_level":"low","risk_rationale":"` + rationale + `"}`
		if _, stripped := sanitizeFabricatedApprovalJSON(raw); stripped {
			t.Errorf("rationale %q claims no human approval and must survive", rationale)
		}
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
	// The second argument is the already-shown carried side, so its ID is the
	// identity that survives a restatement; the surviving item still reports
	// the fresh side's current line.
	if merged.Items[0].ID != "dismissed-2" {
		t.Fatalf("unexpected merged findings: %#v", merged.Items)
	}
	if merged.Items[0].Line != 42 {
		t.Fatalf("expected the fresh side's current line, got %#v", merged.Items[0])
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

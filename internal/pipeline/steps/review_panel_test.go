package steps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func report(name string, f Findings) reviewerReport {
	return reviewerReport{Name: name, Findings: f}
}

func TestCombineReviewerFindings_UnionPreservesItems(t *testing.T) {
	reports := []reviewerReport{
		report("codex", Findings{
			Items: []Finding{
				{ID: "review-codex-1", Severity: "warning", Description: "codex A", Source: "codex"},
				{ID: "review-codex-2", Severity: "info", Description: "codex B", Source: "codex"},
			},
		}),
		report("claude", Findings{
			Items: []Finding{
				{ID: "review-claude-1", Severity: "error", Description: "claude A", Source: "claude"},
			},
		}),
	}

	merged := combineReviewerFindings(reports)

	if len(merged.Items) != 3 {
		t.Fatalf("expected 3 items in the union, got %d", len(merged.Items))
	}
	// Deterministic order: all of codex's items first, then claude's.
	wantIDs := []string{"review-codex-1", "review-codex-2", "review-claude-1"}
	wantSources := []string{"codex", "codex", "claude"}
	for i, item := range merged.Items {
		if item.ID != wantIDs[i] {
			t.Errorf("item %d id = %q, want %q", i, item.ID, wantIDs[i])
		}
		if item.Source != wantSources[i] {
			t.Errorf("item %d source = %q, want %q", i, item.Source, wantSources[i])
		}
	}
}

func TestCombineReviewerFindings_RiskLevelMax(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want string
	}{
		{"low+high", "low", "high", "high"},
		{"low+medium", "low", "medium", "medium"},
		{"medium+low", "medium", "low", "medium"},
		{"empty+low", "", "low", "low"},
		{"high+empty", "high", "", "high"},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := combineReviewerFindings([]reviewerReport{
				report("codex", Findings{RiskLevel: tc.a}),
				report("claude", Findings{RiskLevel: tc.b}),
			})
			if merged.RiskLevel != tc.want {
				t.Errorf("RiskLevel = %q, want %q", merged.RiskLevel, tc.want)
			}
		})
	}
}

func TestCombineReviewerFindings_LabeledScalars(t *testing.T) {
	merged := combineReviewerFindings([]reviewerReport{
		report("codex", Findings{Summary: "codex summary", RiskRationale: "codex rationale"}),
		report("claude", Findings{Summary: "claude summary", RiskRationale: "claude rationale"}),
	})
	if merged.Summary != "[codex] codex summary; [claude] claude summary" {
		t.Errorf("Summary = %q", merged.Summary)
	}
	if merged.RiskRationale != "[codex] codex rationale; [claude] claude rationale" {
		t.Errorf("RiskRationale = %q", merged.RiskRationale)
	}
}

func TestCombineReviewerFindings_SkipsEmptyScalars(t *testing.T) {
	merged := combineReviewerFindings([]reviewerReport{
		report("codex", Findings{Summary: "only codex", RiskRationale: ""}),
		report("claude", Findings{Summary: "", RiskRationale: "only claude"}),
	})
	if merged.Summary != "[codex] only codex" {
		t.Errorf("Summary = %q, want only codex labeled", merged.Summary)
	}
	if merged.RiskRationale != "[claude] only claude" {
		t.Errorf("RiskRationale = %q, want only claude labeled", merged.RiskRationale)
	}
}

func TestCombineReviewerFindings_EmptyAndSingle(t *testing.T) {
	if merged := combineReviewerFindings(nil); len(merged.Items) != 0 || merged.Summary != "" || merged.RiskLevel != "" {
		t.Errorf("empty reports should yield zero-value findings, got %+v", merged)
	}

	single := combineReviewerFindings([]reviewerReport{
		report("codex", Findings{
			Items:         []Finding{{ID: "review-codex-1", Severity: "warning", Description: "x", Source: "codex"}},
			RiskLevel:     "medium",
			RiskRationale: "rationale",
			Summary:       "summary",
		}),
	})
	if len(single.Items) != 1 || single.Items[0].ID != "review-codex-1" || single.Items[0].Source != "codex" {
		t.Errorf("single report item not preserved: %+v", single.Items)
	}
	if single.RiskLevel != "medium" {
		t.Errorf("single report RiskLevel = %q, want medium", single.RiskLevel)
	}
	if single.Summary != "[codex] summary" || single.RiskRationale != "[codex] rationale" {
		t.Errorf("single report scalars = %q / %q", single.Summary, single.RiskRationale)
	}
}

func fanResult(name string, out json.RawMessage, err error) agent.FanOutResult {
	res := &agent.Result{Output: out}
	if err != nil {
		res = nil
	}
	return agent.FanOutResult{Agent: &mockAgent{name: name}, Result: res, Err: err}
}

func TestProcessReviewerResults_NamespacesAndStampsSource(t *testing.T) {
	codexOut, _ := json.Marshal(Findings{
		Items:     []Finding{{Severity: "warning", Description: "no id here", Action: "auto-fix"}},
		RiskLevel: "medium",
	})
	claudeOut, _ := json.Marshal(Findings{
		Items:     []Finding{{ID: "keep-me", Severity: "error", Description: "has id", Action: "ask-user"}},
		RiskLevel: "high",
	})

	var fileLogs []string
	reports, err := processReviewerResults(
		[]agent.FanOutResult{fanResult("codex", codexOut, nil), fanResult("claude", claudeOut, nil)},
		false,
		func(string) {},
		func(s string) { fileLogs = append(fileLogs, s) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}
	// Every id is force-namespaced to review-<name>-<slot>-N; a
	// model-supplied id ("keep-me") is discarded so it cannot collide.
	if got := reports[0].Findings.Items[0].ID; got != "review-codex-1-1" {
		t.Errorf("codex id = %q, want review-codex-1-1", got)
	}
	if got := reports[1].Findings.Items[0].ID; got != "review-claude-2-1" {
		t.Errorf("claude id = %q, want review-claude-2-1 (model id discarded)", got)
	}
	// Source is stamped with the reviewer name on every item.
	if reports[0].Findings.Items[0].Source != "codex" {
		t.Errorf("codex source = %q", reports[0].Findings.Items[0].Source)
	}
	if reports[1].Findings.Items[0].Source != "claude" {
		t.Errorf("claude source = %q", reports[1].Findings.Items[0].Source)
	}
	// Each reviewer's raw report is written to the file-only audit log.
	if len(fileLogs) != 2 {
		t.Fatalf("expected 2 audit log lines, got %d: %v", len(fileLogs), fileLogs)
	}
	if !strings.Contains(fileLogs[0], "[reviewer codex] report:") {
		t.Errorf("first audit line = %q", fileLogs[0])
	}
}

func TestProcessReviewerResults_SameFamilyIDsCollisionFree(t *testing.T) {
	// Two reviewers of the same family, each emitting its own colliding ids,
	// must end up with disjoint namespaced ids so gate selection/instructions
	// target the right finding.
	a, _ := json.Marshal(Findings{Items: []Finding{
		{ID: "review-1", Severity: "warning", Description: "a1"},
		{ID: "review-1", Severity: "info", Description: "a2"},
	}})
	b, _ := json.Marshal(Findings{Items: []Finding{
		{ID: "review-1", Severity: "error", Description: "b1"},
	}})
	reports, err := processReviewerResults(
		[]agent.FanOutResult{fanResult("codex", a, nil), fanResult("codex", b, nil)},
		false,
		func(string) {},
		func(string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range reports {
		for _, item := range r.Findings.Items {
			if item.ID == "" {
				t.Errorf("finding has empty id: %+v", item)
			}
			if seen[item.ID] {
				t.Errorf("duplicate finding id across same-family reviewers: %q", item.ID)
			}
			seen[item.ID] = true
		}
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct ids, got %d: %v", len(seen), seen)
	}
}

func TestProcessReviewerResults_SlotIndexStableUnderFailOpen(t *testing.T) {
	// When fail_open drops an earlier reviewer, the surviving reviewers must keep
	// the namespace tied to their input slot, not the success count, so finding
	// IDs stay stable for selection/instructions keyed by ID across rounds.
	first, _ := json.Marshal(Findings{Items: []Finding{{Severity: "info", Description: "first"}}})
	third, _ := json.Marshal(Findings{Items: []Finding{{Severity: "warning", Description: "third"}}})

	// All three succeed: claude is slot 3.
	all, err := processReviewerResults(
		[]agent.FanOutResult{
			fanResult("codex", first, nil),
			fanResult("rovodev", first, nil),
			fanResult("claude", third, nil),
		},
		true, func(string) {}, func(string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := all[2].Findings.Items[0].ID; got != "review-claude-3-1" {
		t.Fatalf("claude id with all reviewers = %q, want review-claude-3-1", got)
	}

	// The slot-2 reviewer fails: claude must STILL be slot 3, not renumbered to 2.
	dropped, err := processReviewerResults(
		[]agent.FanOutResult{
			fanResult("codex", first, nil),
			fanResult("rovodev", nil, errors.New("boom")),
			fanResult("claude", third, nil),
		},
		true, func(string) {}, func(string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 2 {
		t.Fatalf("expected 2 surviving reports, got %d", len(dropped))
	}
	if got := dropped[1].Findings.Items[0].ID; got != "review-claude-3-1" {
		t.Errorf("claude id after dropping slot 2 = %q, want stable review-claude-3-1", got)
	}
}

func TestCombineReviewerFindings_PreservesTestedTestingSummaryArtifacts(t *testing.T) {
	merged := combineReviewerFindings([]reviewerReport{
		report("codex", Findings{
			Tested:         []string{"unit"},
			TestingSummary: "ran units",
			Artifacts:      []types.TestArtifact{{Label: "codex-log", Path: "a.log"}},
		}),
		report("claude", Findings{
			Tested:         []string{"e2e"},
			TestingSummary: "ran e2e",
			Artifacts:      []types.TestArtifact{{Label: "claude-log", Path: "b.log"}},
		}),
	})
	if got := strings.Join(merged.Tested, ","); got != "unit,e2e" {
		t.Errorf("Tested = %q, want unit,e2e", got)
	}
	if merged.TestingSummary != "[codex] ran units; [claude] ran e2e" {
		t.Errorf("TestingSummary = %q", merged.TestingSummary)
	}
	if len(merged.Artifacts) != 2 || merged.Artifacts[0].Label != "codex-log" || merged.Artifacts[1].Label != "claude-log" {
		t.Errorf("Artifacts not concatenated in reviewer order: %+v", merged.Artifacts)
	}
}

func TestProcessReviewerResults_FailClosed(t *testing.T) {
	ok, _ := json.Marshal(Findings{Items: []Finding{{Severity: "info", Description: "ok"}}})
	_, err := processReviewerResults(
		[]agent.FanOutResult{
			fanResult("codex", ok, nil),
			fanResult("claude", nil, errors.New("boom")),
		},
		false, // fail-closed
		func(string) {},
		func(string) {},
	)
	if err == nil {
		t.Fatal("expected fail-closed error when a reviewer fails")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the failed reviewer family, got %q", err)
	}
}

func TestProcessReviewerResults_FailOpenDropsAndContinues(t *testing.T) {
	ok, _ := json.Marshal(Findings{Items: []Finding{{Severity: "info", Description: "ok"}}})
	var warnings []string
	reports, err := processReviewerResults(
		[]agent.FanOutResult{
			fanResult("codex", ok, nil),
			fanResult("claude", nil, errors.New("boom")),
		},
		true, // fail-open
		func(s string) { warnings = append(warnings, s) },
		func(string) {},
	)
	if err != nil {
		t.Fatalf("fail-open should not error when one reviewer succeeds: %v", err)
	}
	if len(reports) != 1 || reports[0].Name != "codex" {
		t.Fatalf("expected only the codex report to survive, got %+v", reports)
	}
	loud := false
	for _, w := range warnings {
		if strings.Contains(w, "claude") && strings.Contains(w, "DROPPED") {
			loud = true
		}
	}
	if !loud {
		t.Errorf("expected a loud warning naming the dropped reviewer, got %v", warnings)
	}
}

func TestProcessReviewerResults_FailOpenAllFail(t *testing.T) {
	_, err := processReviewerResults(
		[]agent.FanOutResult{
			fanResult("codex", nil, errors.New("boom1")),
			fanResult("claude", nil, errors.New("boom2")),
		},
		true, // fail-open, but nobody succeeds
		func(string) {},
		func(string) {},
	)
	if err == nil {
		t.Fatal("expected an error when every reviewer fails even under fail-open")
	}
}

func TestRiskRankAndSeverityRank(t *testing.T) {
	if !(types.RiskRank("low") < types.RiskRank("medium") && types.RiskRank("medium") < types.RiskRank("high")) {
		t.Error("expected low < medium < high risk ranks")
	}
	if types.RiskRank("") != 0 || types.RiskRank("bogus") != 0 {
		t.Error("expected unknown/empty risk to rank lowest")
	}
	if !(types.SeverityRank("info") < types.SeverityRank("warning") && types.SeverityRank("warning") < types.SeverityRank("error")) {
		t.Error("expected info < warning < error severity ranks")
	}
	if types.SeverityRank("") != 0 || types.SeverityRank("bogus") != 0 {
		t.Error("expected unknown/empty severity to rank lowest")
	}
}

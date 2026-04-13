package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseFindingsJSON_RiskFields(t *testing.T) {
	raw := `{"findings":[{"severity":"error","description":"bug"}],"risk_level":"high","risk_rationale":"Critical bug."}`
	f, err := ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.RiskLevel != "high" {
		t.Errorf("RiskLevel = %q, want %q", f.RiskLevel, "high")
	}
	if f.RiskRationale != "Critical bug." {
		t.Errorf("RiskRationale = %q, want %q", f.RiskRationale, "Critical bug.")
	}
	if len(f.Items) != 1 {
		t.Fatalf("Items count = %d, want 1", len(f.Items))
	}
}

func TestParseFindingsJSON_NoRiskFields(t *testing.T) {
	raw := `{"findings":[{"severity":"info","description":"note"}],"summary":"ok"}`
	f, err := ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.RiskLevel != "" {
		t.Errorf("RiskLevel = %q, want empty", f.RiskLevel)
	}
	if f.RiskRationale != "" {
		t.Errorf("RiskRationale = %q, want empty", f.RiskRationale)
	}
	if f.Summary != "ok" {
		t.Errorf("Summary = %q, want %q", f.Summary, "ok")
	}
}

func TestFilterFindings_PreservesRiskFields(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{ID: "f1", Severity: "error", Description: "bad"},
			{ID: "f2", Severity: "warning", Description: "warn"},
		},
		Summary:       "2 issues",
		RiskLevel:     "medium",
		RiskRationale: "Some risk.",
	}
	filtered := FilterFindings(f, []string{"f1"})
	if filtered.RiskLevel != "medium" {
		t.Errorf("RiskLevel = %q, want %q", filtered.RiskLevel, "medium")
	}
	if filtered.RiskRationale != "Some risk." {
		t.Errorf("RiskRationale = %q, want %q", filtered.RiskRationale, "Some risk.")
	}
	if len(filtered.Items) != 1 {
		t.Fatalf("Items count = %d, want 1", len(filtered.Items))
	}
	if filtered.Items[0].ID != "f1" {
		t.Errorf("filtered item ID = %q, want %q", filtered.Items[0].ID, "f1")
	}
}

func TestExcludeFindings_KeepsUnselected(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{ID: "f1", Severity: "error", Description: "bad"},
			{ID: "f2", Severity: "warning", Description: "warn"},
			{ID: "f3", Severity: "info", Description: "note"},
		},
		Summary:       "3 issues",
		RiskLevel:     "medium",
		RiskRationale: "Some risk.",
	}
	excluded := ExcludeFindings(f, []string{"f1", "f3"})
	if len(excluded.Items) != 1 {
		t.Fatalf("Items count = %d, want 1", len(excluded.Items))
	}
	if excluded.Items[0].ID != "f2" {
		t.Errorf("excluded item ID = %q, want %q", excluded.Items[0].ID, "f2")
	}
	if excluded.RiskLevel != "medium" {
		t.Errorf("RiskLevel = %q, want %q", excluded.RiskLevel, "medium")
	}
}

func TestExcludeFindings_AllExcluded(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{ID: "f1", Severity: "error", Description: "bad"},
		},
		RiskLevel: "high",
	}
	excluded := ExcludeFindings(f, []string{"f1"})
	if len(excluded.Items) != 0 {
		t.Errorf("Items count = %d, want 0", len(excluded.Items))
	}
}

func TestExcludeFindings_NoneExcluded(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{ID: "f1", Severity: "error", Description: "bad"},
		},
		RiskLevel: "low",
	}
	excluded := ExcludeFindings(f, []string{})
	if len(excluded.Items) != 1 {
		t.Errorf("Items count = %d, want 1", len(excluded.Items))
	}
}

func TestFilterFindings_EmptyIDs(t *testing.T) {
	f := Findings{
		Items:         []Finding{{ID: "f1", Severity: "error", Description: "bad"}},
		RiskLevel:     "low",
		RiskRationale: "Safe.",
	}
	filtered := FilterFindings(f, []string{})
	if len(filtered.Items) != 1 {
		t.Errorf("expected all items returned for empty IDs, got %d", len(filtered.Items))
	}
	if filtered.RiskLevel != "low" {
		t.Errorf("RiskLevel = %q, want %q", filtered.RiskLevel, "low")
	}
}

func TestParseFindingsJSON_Action(t *testing.T) {
	raw := `{"findings":[{"severity":"warning","description":"design choice","action":"ask-user"},{"severity":"error","description":"bug","action":"auto-fix"}],"risk_level":"medium","risk_rationale":"Mixed."}`
	f, err := ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Items) != 2 {
		t.Fatalf("Items count = %d, want 2", len(f.Items))
	}
	if f.Items[0].Action != ActionAskUser {
		t.Errorf("Items[0].Action = %q, want %q", f.Items[0].Action, ActionAskUser)
	}
	if f.Items[1].Action != ActionAutoFix {
		t.Errorf("Items[1].Action = %q, want %q", f.Items[1].Action, ActionAutoFix)
	}
}

func TestAutoFixableFindings_FiltersToAutoFix(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{ID: "f1", Severity: "error", Description: "bug", Action: ActionAutoFix},
			{ID: "f2", Severity: "warning", Description: "design choice", Action: ActionAskUser},
			{ID: "f3", Severity: "warning", Description: "missing check", Action: ActionAutoFix},
			{ID: "f4", Severity: "info", Description: "note", Action: ActionNoOp},
		},
		RiskLevel: "medium",
	}
	fixable := AutoFixableFindings(f)
	if len(fixable.Items) != 2 {
		t.Fatalf("Items count = %d, want 2", len(fixable.Items))
	}
	if fixable.Items[0].ID != "f1" {
		t.Errorf("Items[0].ID = %q, want %q", fixable.Items[0].ID, "f1")
	}
	if fixable.Items[1].ID != "f3" {
		t.Errorf("Items[1].ID = %q, want %q", fixable.Items[1].ID, "f3")
	}
}

func TestAutoFixableFindings_AllAskUser(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{ID: "f1", Severity: "warning", Description: "choice", Action: ActionAskUser},
		},
	}
	fixable := AutoFixableFindings(f)
	if len(fixable.Items) != 0 {
		t.Errorf("Items count = %d, want 0", len(fixable.Items))
	}
}

func TestAutoFixableFindings_NoOpExcluded(t *testing.T) {
	f := Findings{
		Items: []Finding{
			{ID: "f1", Severity: "info", Description: "note", Action: ActionNoOp},
			{ID: "f2", Severity: "info", Description: "fyi", Action: ActionNoOp},
		},
	}
	fixable := AutoFixableFindings(f)
	if len(fixable.Items) != 0 {
		t.Errorf("Items count = %d, want 0", len(fixable.Items))
	}
}

func TestHasAskUserFindings(t *testing.T) {
	tests := []struct {
		name   string
		items  []Finding
		expect bool
	}{
		{"has ask-user", []Finding{{Action: ActionAskUser}}, true},
		{"only auto-fix", []Finding{{Action: ActionAutoFix}}, false},
		{"only no-op", []Finding{{Action: ActionNoOp}}, false},
		{"mixed", []Finding{{Action: ActionAutoFix}, {Action: ActionAskUser}}, true},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Findings{Items: tt.items}
			if got := HasAskUserFindings(f); got != tt.expect {
				t.Errorf("HasAskUserFindings() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestMarshalFindingsJSON_AlwaysIncludesRiskFields(t *testing.T) {
	f := Findings{
		Items:   []Finding{{Severity: "info", Description: "note"}},
		Summary: "ok",
	}
	raw, err := MarshalFindingsJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"risk_level"`) {
		t.Errorf("expected risk_level to be present even when empty, got %s", raw)
	}
	if !strings.Contains(raw, `"risk_rationale"`) {
		t.Errorf("expected risk_rationale to be present even when empty, got %s", raw)
	}
}

func TestFinding_Action_SerializedWhenEmpty(t *testing.T) {
	f := Finding{Severity: "error", Description: "bug", Action: ""}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"action":`) {
		t.Errorf("expected action to be present, got %s", s)
	}
}

func TestFinding_Action_Values(t *testing.T) {
	for _, action := range []string{ActionNoOp, ActionAutoFix, ActionAskUser} {
		f := Finding{Severity: "error", Description: "test", Action: action}
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		if !strings.Contains(s, fmt.Sprintf(`"action":"%s"`, action)) {
			t.Errorf("expected action %q in output, got %s", action, s)
		}
	}
}

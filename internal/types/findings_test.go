package types

import "testing"

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

package types

import (
	"encoding/json"
	"fmt"
)

// Finding represents a single review, test, lint, or PR comment finding.
type Finding struct {
	ID                  string `json:"id,omitempty"`
	Severity            string `json:"severity"`
	File                string `json:"file,omitempty"`
	Line                int    `json:"line,omitempty"`
	Description         string `json:"description"`
	RequiresHumanReview bool   `json:"requires_human_review"`
}

// Findings is the structured findings payload exchanged across pipeline, IPC, and TUI.
type Findings struct {
	Items         []Finding `json:"findings"`
	Summary       string    `json:"summary"`
	RiskLevel     string    `json:"risk_level"`
	RiskRationale string    `json:"risk_rationale"`
}

type findingsWire struct {
	Items         []Finding `json:"findings"`
	Legacy        []Finding `json:"items"`
	Summary       string    `json:"summary"`
	RiskLevel     string    `json:"risk_level"`
	RiskRationale string    `json:"risk_rationale"`
}

// ParseFindingsJSON decodes findings JSON, accepting both current and legacy item keys.
func ParseFindingsJSON(raw string) (Findings, error) {
	var wire findingsWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return Findings{}, err
	}
	items := wire.Items
	if len(items) == 0 && len(wire.Legacy) > 0 {
		items = wire.Legacy
	}
	return Findings{Items: items, Summary: wire.Summary, RiskLevel: wire.RiskLevel, RiskRationale: wire.RiskRationale}, nil
}

// NormalizeFindings assigns deterministic IDs to findings that do not have one yet.
func NormalizeFindings(findings Findings, prefix string) Findings {
	for i := range findings.Items {
		if findings.Items[i].ID != "" {
			continue
		}
		findings.Items[i].ID = prefix + "-" + itoa(i+1)
	}
	return findings
}

// FilterFindings keeps only findings whose IDs are included in ids.
func FilterFindings(findings Findings, ids []string) Findings {
	if len(ids) == 0 {
		return findings
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	filtered := Findings{Summary: findings.Summary, RiskLevel: findings.RiskLevel, RiskRationale: findings.RiskRationale}
	for _, item := range findings.Items {
		if selected[item.ID] {
			filtered.Items = append(filtered.Items, item)
		}
	}
	if len(filtered.Items) != len(findings.Items) {
		filtered.Summary = summarizeSelectedFindings(len(filtered.Items))
	}
	return filtered
}

// ExcludeFindings keeps only findings whose IDs are NOT in the excluded set.
func ExcludeFindings(findings Findings, ids []string) Findings {
	if len(ids) == 0 {
		return findings
	}
	excluded := make(map[string]bool, len(ids))
	for _, id := range ids {
		excluded[id] = true
	}
	result := Findings{Summary: findings.Summary, RiskLevel: findings.RiskLevel, RiskRationale: findings.RiskRationale}
	for _, item := range findings.Items {
		if !excluded[item.ID] {
			result.Items = append(result.Items, item)
		}
	}
	return result
}

// AutoFixableFindings returns a new Findings containing only items where
// RequiresHumanReview is false. These are safe for automatic fixing without
// user involvement.
func AutoFixableFindings(findings Findings) Findings {
	result := Findings{Summary: findings.Summary, RiskLevel: findings.RiskLevel, RiskRationale: findings.RiskRationale}
	for _, item := range findings.Items {
		if !item.RequiresHumanReview {
			result.Items = append(result.Items, item)
		}
	}
	return result
}

func HasHumanReviewFindings(findings Findings) bool {
	for _, item := range findings.Items {
		if item.RequiresHumanReview {
			return true
		}
	}
	return false
}

func summarizeSelectedFindings(count int) string {
	switch count {
	case 0:
		return "0 selected findings"
	case 1:
		return "1 selected finding"
	default:
		return fmt.Sprintf("%d selected findings", count)
	}
}

// MarshalFindingsJSON encodes findings using the current wire shape.
func MarshalFindingsJSON(findings Findings) (string, error) {
	raw, err := json.Marshal(findings)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

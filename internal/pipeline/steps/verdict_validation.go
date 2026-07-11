package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type findingsOutputEnvelope struct {
	Findings       *[]json.RawMessage    `json:"findings"`
	Summary        *string               `json:"summary"`
	Tested         *[]string             `json:"tested"`
	TestingSummary *string               `json:"testing_summary"`
	Artifacts      *[]types.TestArtifact `json:"artifacts"`
	RiskLevel      *string               `json:"risk_level"`
	RiskRationale  *string               `json:"risk_rationale"`
}

func decodeFindingsOutput(raw []byte) (Findings, findingsOutputEnvelope, error) {
	if len(raw) == 0 {
		return Findings{}, findingsOutputEnvelope{}, fmt.Errorf("missing structured findings output")
	}
	var envelope findingsOutputEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Findings{}, findingsOutputEnvelope{}, fmt.Errorf("malformed structured findings output: %w", err)
	}
	if envelope.Findings == nil {
		return Findings{}, findingsOutputEnvelope{}, fmt.Errorf("missing findings array")
	}
	findings, err := types.ParseFindingsJSON(string(raw))
	if err != nil {
		return Findings{}, findingsOutputEnvelope{}, fmt.Errorf("parse structured findings output: %w", err)
	}
	if err := validateFindingItems(findings.Items); err != nil {
		return Findings{}, findingsOutputEnvelope{}, err
	}
	return findings, envelope, nil
}

// validateFindingsOutput enforces the semantic guarantees of findingsSchema at
// the publication boundary instead of relying only on an adapter's schema check.
func validateFindingsOutput(raw []byte) (Findings, error) {
	findings, envelope, err := decodeFindingsOutput(raw)
	if err != nil {
		return Findings{}, err
	}
	if envelope.Summary == nil || strings.TrimSpace(*envelope.Summary) == "" {
		return Findings{}, fmt.Errorf("missing summary")
	}
	return findings, nil
}

func validateReviewFindingsOutput(raw []byte) (Findings, error) {
	findings, envelope, err := decodeFindingsOutput(raw)
	if err != nil {
		return Findings{}, err
	}
	if envelope.RiskLevel == nil {
		return Findings{}, fmt.Errorf("missing risk level")
	}
	switch strings.TrimSpace(*envelope.RiskLevel) {
	case "low", "medium", "high":
	default:
		return Findings{}, fmt.Errorf("invalid risk level %q", strings.TrimSpace(*envelope.RiskLevel))
	}
	if envelope.RiskRationale == nil || strings.TrimSpace(*envelope.RiskRationale) == "" {
		return Findings{}, fmt.Errorf("missing risk rationale")
	}
	return findings, nil
}

func validateTestFindingsOutput(raw []byte) (Findings, error) {
	findings, envelope, err := decodeFindingsOutput(raw)
	if err != nil {
		return Findings{}, err
	}
	if envelope.Summary == nil || strings.TrimSpace(*envelope.Summary) == "" {
		return Findings{}, fmt.Errorf("missing test evidence summary")
	}
	if envelope.Tested == nil || len(*envelope.Tested) == 0 {
		return Findings{}, fmt.Errorf("missing tested evidence")
	}
	for i, tested := range *envelope.Tested {
		if strings.TrimSpace(tested) == "" {
			return Findings{}, fmt.Errorf("tested evidence %d is empty", i)
		}
	}
	if envelope.TestingSummary == nil || strings.TrimSpace(*envelope.TestingSummary) == "" {
		return Findings{}, fmt.Errorf("missing testing summary")
	}
	if envelope.Artifacts == nil {
		return Findings{}, fmt.Errorf("missing artifacts array")
	}
	for i, artifact := range *envelope.Artifacts {
		if strings.TrimSpace(artifact.Label) == "" {
			return Findings{}, fmt.Errorf("artifact %d missing label", i)
		}
	}
	return findings, nil
}

func validateFindingItems(items []Finding) error {
	for i, item := range items {
		if strings.TrimSpace(item.Severity) == "" {
			return fmt.Errorf("finding %d missing severity", i)
		}
		if strings.TrimSpace(item.Description) == "" {
			return fmt.Errorf("finding %d missing description", i)
		}
		if strings.TrimSpace(item.Action) == "" {
			return fmt.Errorf("finding %d missing action", i)
		}
	}
	return nil
}

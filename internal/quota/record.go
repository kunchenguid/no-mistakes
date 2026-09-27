package quota

import (
	"encoding/json"
	"strings"
	"time"
)

// maxEvidenceText bounds one free-text field inside a recorded payload. The
// payload is written alongside the run's other durable state, so a refusal reason
// must never be the thing that makes a row unusable.
const maxEvidenceText = 240

// EvidencePayload renders a routing decision as the bounded JSON persisted on the
// run.
//
// It is a restatement of the decision rather than the vendor's report: the chosen
// candidate's standing, and one verdict per candidate with the gate that decided
// it. That keeps the recorded evidence auditable (an operator can see why the run
// did not use the harness they expected) while staying small enough that a run's
// records never carry an unbounded vendor payload.
func (r Record) EvidencePayload() string {
	payload := evidencePayload{
		Step:        string(r.Step),
		Reason:      clipEvidenceText(r.Reason),
		Keeping:     r.Decision.Kept,
		Agent:       string(r.Decision.Candidate.Agent),
		Provider:    clipEvidenceText(r.Decision.EvidenceProvider),
		Model:       clipEvidenceText(r.Decision.Row.ID),
		Attestation: clipEvidenceText(r.Decision.Attestation),
		Standing:    clipEvidenceText(r.Decision.Row.Standings()),
		ReadError:   clipEvidenceText(r.ReadError),
	}
	generated := r.Generated
	if generated.IsZero() {
		generated = r.Decision.GeneratedAt
	}
	if !generated.IsZero() {
		payload.GeneratedAt = generated.UTC().Format("2006-01-02T15:04:05Z")
	}
	for _, assessment := range r.Decision.Assessments {
		verdict := candidateVerdict{
			Candidate:   clipEvidenceText(assessment.Candidate.String()),
			Verdict:     "eligible",
			Percent:     assessment.Row.PercentRemaining,
			Spend:       assessment.Row.SpendPriority,
			Runway:      clipEvidenceText(runwaySummary(assessment.Row)),
			EvidenceVia: clipEvidenceText(assessment.EvidenceProvider),
		}
		if !assessment.Eligible() {
			verdict.Verdict = string(assessment.Gate)
			verdict.Reason = clipEvidenceText(assessment.Reason)
		}
		payload.Candidates = append(payload.Candidates, verdict)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// The payload is assembled from primitives, so this is unreachable; an
		// unencodable payload must still not fail a routing decision.
		return `{"error":"encode quota evidence"}`
	}
	return string(encoded)
}

func runwaySummary(row ModelEvidence) string {
	if row.RunwayStatus == "" {
		return ""
	}
	summary := row.RunwayStatus
	if row.RunwaySeconds != nil {
		summary += " " + HumanDuration(time.Duration(*row.RunwaySeconds)*time.Second)
	}
	if row.Confidence != "" {
		summary += " (" + row.Confidence + ")"
	}
	return summary
}

func clipEvidenceText(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= maxEvidenceText {
		return trimmed
	}
	return trimmed[:maxEvidenceText] + "..."
}

type evidencePayload struct {
	Step        string             `json:"step"`
	Reason      string             `json:"reason"`
	GeneratedAt string             `json:"generated_at,omitempty"`
	Agent       string             `json:"agent,omitempty"`
	Provider    string             `json:"provider,omitempty"`
	Model       string             `json:"model,omitempty"`
	Attestation string             `json:"attestation,omitempty"`
	Standing    string             `json:"standing,omitempty"`
	Keeping     bool               `json:"kept,omitempty"`
	Candidates  []candidateVerdict `json:"candidates"`
	ReadError   string             `json:"read_error,omitempty"`
}

type candidateVerdict struct {
	Candidate   string   `json:"candidate"`
	Verdict     string   `json:"verdict"`
	Reason      string   `json:"reason,omitempty"`
	Percent     *int     `json:"percent_remaining,omitempty"`
	Spend       *float64 `json:"spend_priority,omitempty"`
	Runway      string   `json:"runway,omitempty"`
	EvidenceVia string   `json:"evidence_provider,omitempty"`
}

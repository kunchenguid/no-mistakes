package types

import "testing"

// TestParseFindingsJSON_RoundTripsEvidenceSource pins the diff-class gate's
// two recorded fields through the wire shape. findingsWire copies fields
// explicitly, so a field added to Findings alone would silently drop on every
// parse - which is how axi, the PR body, and the reuse lookup would all stop
// seeing which path the Test step took.
func TestParseFindingsJSON_RoundTripsEvidenceSource(t *testing.T) {
	t.Parallel()
	encoded, err := MarshalFindingsJSON(Findings{
		Verdict:        TestVerdictGo,
		TestedHeadSHA:  "abc123",
		EvidenceSource: TestEvidenceSourceReused,
		EvidenceReason: "product files unchanged since abc123; reused from run run-9",
	})
	if err != nil {
		t.Fatalf("MarshalFindingsJSON() error = %v", err)
	}
	parsed, err := ParseFindingsJSON(encoded)
	if err != nil {
		t.Fatalf("ParseFindingsJSON() error = %v", err)
	}
	if parsed.EvidenceSource != TestEvidenceSourceReused {
		t.Fatalf("EvidenceSource = %q, want %q", parsed.EvidenceSource, TestEvidenceSourceReused)
	}
	if parsed.EvidenceReason == "" {
		t.Fatal("EvidenceReason was dropped")
	}

	// Payloads recorded before the gate existed carry neither field and must
	// still parse, reading as "the agent ran", which is what those runs did.
	legacy, err := ParseFindingsJSON(`{"findings":[],"summary":"x","verdict":"go","tested_head_sha":"abc123"}`)
	if err != nil {
		t.Fatalf("legacy parse error = %v", err)
	}
	if legacy.EvidenceSource != "" || legacy.EvidenceReason != "" {
		t.Fatalf("legacy payload invented an evidence source: %+v", legacy)
	}
}

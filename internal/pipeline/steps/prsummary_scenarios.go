package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This file owns how the Test step's live-validation contract - the derived
// scenarios and the run's verdict - reaches a human reviewer.
//
// The contract exists because "tests passed" answered a question nobody asked:
// a reviewer wants to know which end-user scenarios were driven against the
// real product, which were only asserted against stubs, and which could not be
// driven here at all. So every surface renders the same three facts per
// scenario (what was exercised, what happened, whether it was live) plus the
// step's own verdict, and the machine-readable half of the same answer rides
// the PR attestation as live_validation for a consumer that must decide
// whether this change was live validated without reading prose.

// testingEvidenceFindings parses the one findings payload the whole Testing
// section reads, so every field below comes from the same record rather than
// from five independent parses that could in principle disagree.
// testingEvidenceFindingsJSON yields at most one payload: the step's own final
// findings when they carry evidence metadata, else the last round that did, so
// a step whose final findings were cleared by a fix still renders the evidence
// its last round captured.
//
// collectTestingArtifacts deliberately stays separate: it needs the rendering
// options and its own de-duplication, and it consumes the raw payload rather
// than one field of it.
func testingEvidenceFindings(sr *db.StepResult, rounds []*db.StepRound) types.Findings {
	for _, raw := range testingEvidenceFindingsJSON(sr, rounds) {
		if raw == nil || strings.TrimSpace(*raw) == "" {
			continue
		}
		findings, err := types.ParseFindingsJSON(*raw)
		if err != nil {
			continue
		}
		return findings
	}
	// The zero value answers "nothing recorded" for every caller below.
	return types.Findings{}
}

// collectTestingScenarios returns the scenarios recorded for the test step.
func collectTestingScenarios(sr *db.StepResult, rounds []*db.StepRound) []types.TestScenario {
	findings := testingEvidenceFindings(sr, rounds)
	return findings.Scenarios
}

// collectTestingVerdict returns the test step's recorded verdict, or an empty
// string when the step predates the contract or recorded none.
func collectTestingVerdict(sr *db.StepResult, rounds []*db.StepRound) string {
	findings := testingEvidenceFindings(sr, rounds)
	if types.IsKnownTestVerdict(findings.Verdict) {
		return findings.Verdict
	}
	return ""
}

// collectTestingEvidenceReason returns the Test step's recorded account of
// which path its diff-class gate took, or "" for a step that predates the gate.
func collectTestingEvidenceReason(sr *db.StepResult, rounds []*db.StepRound) string {
	findings := testingEvidenceFindings(sr, rounds)
	return strings.TrimSpace(findings.EvidenceReason)
}

// collectTestingEvidenceSource returns the Test step's recorded evidence
// source, or "" for a step recorded with the gate off or before it existed.
func collectTestingEvidenceSource(sr *db.StepResult, rounds []*db.StepRound) string {
	findings := testingEvidenceFindings(sr, rounds)
	return strings.TrimSpace(findings.EvidenceSource)
}

// renderLiveValidationLine is the one-line answer to "was this live
// validated": the verdict, how much of the scenario list was actually driven
// against the product, and - because a verdict the agent never re-derived
// reads identically otherwise - which path the diff-class gate took to get
// it. It returns "" when none of the three is recorded.
//
// evidenceSource is what keeps the count honest. A reused verdict's scenarios
// were driven live, but in an EARLIER run, and this line is read as a claim
// about the commit the PR is showing. So a reuse says so in the sentence
// rather than leaving "driven live against the product" to be read as this
// run's work; the attestation makes the same distinction by omitting
// live_validation for a head no agent drove.
func renderLiveValidationLine(scenarios []types.TestScenario, verdict, evidenceReason, evidenceSource string) string {
	live, total := types.LiveScenarioCounts(scenarios)
	evidenceReason = strings.TrimSpace(evidenceReason)
	if !types.IsKnownTestVerdict(verdict) && total == 0 && evidenceReason == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Live validation: ")
	if types.IsKnownTestVerdict(verdict) {
		b.WriteString(verdictEmoji(verdict))
		b.WriteString(" ")
		b.WriteString(verdict)
	} else {
		b.WriteString("no verdict recorded")
	}
	if total > 0 {
		if strings.TrimSpace(evidenceSource) == types.TestEvidenceSourceReused {
			b.WriteString(fmt.Sprintf(" - %d of %d scenarios driven live against the product in the earlier run this verdict comes from, not in this run", live, total))
		} else {
			b.WriteString(fmt.Sprintf(" - %d of %d scenarios driven live against the product", live, total))
		}
	}
	if evidenceReason != "" {
		b.WriteString(" (" + evidenceReason + ")")
	}
	return b.String()
}

func verdictEmoji(verdict string) string {
	switch verdict {
	case types.TestVerdictGo:
		return "✅"
	case types.TestVerdictNoGo:
		return "❌"
	default:
		return "⚠️"
	}
}

func scenarioResultEmoji(result string) string {
	switch result {
	case types.ScenarioResultPass:
		return "✅"
	case types.ScenarioResultFail:
		return "❌"
	default:
		return "⏸️"
	}
}

// renderScenarioTable renders the scenario list as a markdown table. A
// markdown table is used on every host, including the no-HTML Bitbucket
// flavor, because it carries no HTML of its own.
//
// The Evidence column deliberately merges a passing scenario's evidence with
// an untested scenario's reason: a reader wants one column answering "on what
// basis?", and for an untested scenario the unavailable capability or absence
// of a live product surface is that basis.
func renderScenarioTable(scenarios []types.TestScenario, flavor prBodyFlavor) string {
	if len(scenarios) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("| Scenario | Result | Live | Evidence |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	rows := 0
	for _, scenario := range scenarios {
		name := scenarioCell(scenario.Name, flavor)
		if name == "" {
			continue
		}
		result := scenario.Result
		live := "no"
		if scenario.Live {
			live = "live"
		}
		basis := strings.TrimSpace(scenario.Evidence)
		if result == types.ScenarioResultUntested && strings.TrimSpace(scenario.Reason) != "" {
			basis = strings.TrimSpace(scenario.Reason)
		}
		b.WriteString(fmt.Sprintf("| %s | %s %s | %s | %s |\n",
			name,
			scenarioResultEmoji(result),
			result,
			live,
			scenarioCell(basis, flavor),
		))
		rows++
	}
	if rows == 0 {
		return ""
	}
	return b.String()
}

// scenarioCell makes agent-authored text safe inside a table cell: collapsed
// to a single line (a newline would end the row), pipe-escaped (an unescaped
// pipe would invent a column), fold-marker escaped like every other quoted
// agent string, and length-bounded so one verbose scenario cannot dominate the
// PR body.
func scenarioCell(text string, flavor prBodyFlavor) string {
	clean := sanitizePromptText(text)
	if clean == "" {
		return ""
	}
	clean = truncateScenarioCell(clean)
	clean = escapePRText(clean, flavor)
	return strings.ReplaceAll(clean, "|", "\\|")
}

const maxScenarioCellRunes = 200

func truncateScenarioCell(text string) string {
	runes := []rune(text)
	if len(runes) <= maxScenarioCellRunes {
		return text
	}
	return strings.TrimSpace(string(runes[:maxScenarioCellRunes])) + "…"
}

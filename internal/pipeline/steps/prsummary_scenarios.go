package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

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

// collectTestingScenarios returns the scenarios recorded for the test step,
// reading the same findings payload the rest of the Testing section reads so a
// step whose final findings were cleared by a fix still renders the evidence
// its last round captured.
func collectTestingScenarios(sr *db.StepResult, rounds []*db.StepRound) []types.TestScenario {
	for _, raw := range testingEvidenceFindingsJSON(sr, rounds) {
		if raw == nil || strings.TrimSpace(*raw) == "" {
			continue
		}
		findings, err := types.ParseFindingsJSON(*raw)
		if err != nil {
			continue
		}
		if len(findings.Scenarios) > 0 {
			return findings.Scenarios
		}
	}
	return nil
}

// collectTestingVerdict returns the test step's recorded verdict, or an empty
// string when the step predates the contract or recorded none.
func collectTestingVerdict(sr *db.StepResult, rounds []*db.StepRound) string {
	for _, raw := range testingEvidenceFindingsJSON(sr, rounds) {
		if raw == nil || strings.TrimSpace(*raw) == "" {
			continue
		}
		findings, err := types.ParseFindingsJSON(*raw)
		if err != nil {
			continue
		}
		if types.IsKnownTestVerdict(findings.Verdict) {
			return findings.Verdict
		}
	}
	return ""
}

// renderLiveValidationLine is the one-line answer to "was this live
// validated": the verdict plus how much of the scenario list was actually
// driven against the product. It returns "" when neither is recorded.
func renderLiveValidationLine(scenarios []types.TestScenario, verdict string) string {
	live, total := types.LiveScenarioCounts(scenarios)
	if !types.IsKnownTestVerdict(verdict) && total == 0 {
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
		b.WriteString(fmt.Sprintf(" - %d of %d scenarios driven live against the product", live, total))
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
func renderScenarioTable(scenarios []types.TestScenario, opts testingSummaryOptions) string {
	flavor := opts.flavor
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
			scenarioEvidenceCell(basis, opts),
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

func scenarioEvidenceCell(text string, opts testingSummaryOptions) string {
	if opts.evidenceLinkBase == "" || opts.evidenceRoot == "" {
		return scenarioCell(text, opts.flavor)
	}
	clean := truncateScenarioCell(sanitizePromptText(text))
	var b strings.Builder
	for _, token := range strings.FieldsFunc(clean, func(r rune) bool { return r == ';' || r == ',' || unicode.IsSpace(r) }) {
		start := strings.Index(clean, token)
		b.WriteString(clean[:start])
		path := filepath.Join(opts.evidenceRoot, filepath.FromSlash(token))
		link := ""
		if filepath.IsLocal(token) {
			if _, inside := artifactPathRelativeToRoot(path, opts.evidenceRoot); inside {
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
					link = renderEvidenceFileLink(opts.evidenceLinkBase, opts.evidenceRoot, path, token)
				}
			}
		}
		if link != "" {
			b.WriteString(strings.ReplaceAll(link, "|", "\\|"))
		} else {
			b.WriteString(strings.ReplaceAll(escapePRText(token, opts.flavor), "|", "\\|"))
		}
		clean = clean[start+len(token):]
	}
	b.WriteString(clean)
	return escapePipelineFoldMarkers(b.String())
}

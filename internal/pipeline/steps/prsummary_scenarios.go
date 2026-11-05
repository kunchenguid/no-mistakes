package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

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
	// The evidence directory is listed once per table, not once per cell.
	var names []string
	if opts.evidenceLinkBase != "" && opts.evidenceRoot != "" {
		names = evidenceFileNames(opts.evidenceRoot)
	}
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
			scenarioEvidenceCell(basis, opts, names),
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

// scenarioEvidenceCell renders the Evidence column. With link_base_url set,
// every complete reference to a file under the run's evidence directory
// becomes a link: a name as the test agent wrote it, relative to that
// directory (nested paths and spaces included, matched against the files that
// exist, longest first), or an absolute path inside it, linked by its relative
// name. Everything else stays prose. names is evidenceFileNames of the root.
func scenarioEvidenceCell(text string, opts testingSummaryOptions, names []string) string {
	if opts.evidenceLinkBase == "" || opts.evidenceRoot == "" {
		return scenarioCell(text, opts.flavor)
	}
	clean := truncateScenarioCell(sanitizePromptText(text))
	plain := func(s string) string { return strings.ReplaceAll(escapePRText(s, opts.flavor), "|", "\\|") }
	var b, prose strings.Builder
	rest := clean
	for len(rest) > 0 {
		if scenarioReferenceCanStart(clean, len(clean)-len(rest)) {
			if label, path, n := matchScenarioEvidenceReference(rest, opts.evidenceRoot, names); n > 0 {
				if link := renderEvidenceFileLink(opts.evidenceLinkBase, opts.evidenceRoot, path, label); link != "" {
					b.WriteString(plain(prose.String()))
					prose.Reset()
					b.WriteString(strings.ReplaceAll(link, "|", "\\|"))
					rest = rest[n:]
					continue
				}
			}
		}
		r, size := utf8.DecodeRuneInString(rest)
		prose.WriteRune(r)
		rest = rest[size:]
	}
	b.WriteString(plain(prose.String()))
	return escapePipelineFoldMarkers(b.String())
}

// matchScenarioEvidenceReference returns the file reference at the start of
// rest - the longest existing relative name, else an absolute path inside the
// evidence root - as its link label, its filesystem path, and the number of
// bytes it spans; n is 0 when rest starts with no file reference.
func matchScenarioEvidenceReference(rest, root string, names []string) (label, path string, n int) {
	for _, name := range names {
		if strings.HasPrefix(rest, name) && scenarioReferenceCanEnd(rest[len(name):]) {
			return name, filepath.Join(root, filepath.FromSlash(name)), len(name)
		}
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r == ';' || r == ',' || unicode.IsSpace(r) })
	if end < 0 {
		end = len(rest)
	}
	token := strings.TrimRight(rest[:end], ".)]")
	if token == "" || !filepath.IsAbs(token) {
		return "", "", 0
	}
	rel, inside := artifactPathRelativeToRoot(token, root)
	if !inside {
		return "", "", 0
	}
	if info, err := os.Stat(token); err != nil || !info.Mode().IsRegular() {
		return "", "", 0
	}
	return filepath.ToSlash(rel), token, len(token)
}

// scenarioReferenceCanStart reports whether a file reference may begin at
// byte offset pos of text: at the start, or after whitespace or a separator.
func scenarioReferenceCanStart(text string, pos int) bool {
	if pos == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:pos])
	return unicode.IsSpace(r) || strings.ContainsRune(";,([|", r)
}

// scenarioReferenceCanEnd reports whether a file reference that ends where
// after begins is complete: the cell ends, a separator or whitespace follows,
// or a sentence-ending period does.
func scenarioReferenceCanEnd(after string) bool {
	if after == "" {
		return true
	}
	r, size := utf8.DecodeRuneInString(after)
	if unicode.IsSpace(r) || strings.ContainsRune(";,)]|", r) {
		return true
	}
	if r != '.' {
		return false
	}
	next := after[size:]
	if next == "" {
		return true
	}
	r, _ = utf8.DecodeRuneInString(next)
	return unicode.IsSpace(r)
}

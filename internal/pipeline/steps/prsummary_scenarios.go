package steps

import (
	"fmt"
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
	rendered, _ := renderScenarioEvidenceReferences(clean, opts, names, true)
	return escapePipelineFoldMarkers(rendered)
}

func renderScenarioEvidenceReferences(clean string, opts testingSummaryOptions, names []string, codeSpans bool) (string, bool) {
	linked := false
	plain := func(s string) string { return strings.ReplaceAll(escapePRText(s, opts.flavor), "|", "\\|") }
	var b, prose strings.Builder
	rest := clean
	for len(rest) > 0 {
		if codeSpans {
			if content, n := scenarioCodeSpan(rest); n > 0 {
				trimmed := strings.TrimSpace(content)
				if _, _, m := matchScenarioEvidenceReference(trimmed, opts.evidenceRoot, names); m > 0 && m == len(trimmed) {
					content = trimmed
				}
				if rendered, found := renderScenarioEvidenceReferences(content, opts, names, false); found {
					b.WriteString(plain(prose.String()))
					prose.Reset()
					b.WriteString(rendered)
					linked = true
				} else {
					prose.WriteString(rest[:n])
				}
				rest = rest[n:]
				continue
			}
		}
		// A reference the cell already carries as a link is complete:
		// rewriting a filename inside it would break the link.
		if n := scenarioExistingReferenceSpan(rest); n > 0 {
			prose.WriteString(rest[:n])
			rest = rest[n:]
			continue
		}
		if scenarioReferenceCanStart(clean, len(clean)-len(rest)) {
			if label, path, n := matchScenarioEvidenceReference(rest, opts.evidenceRoot, names); n > 0 {
				if link := renderEvidenceFileLink(opts.evidenceLinkBase, opts.evidenceRoot, path, label); link != "" {
					b.WriteString(plain(prose.String()))
					prose.Reset()
					b.WriteString(strings.ReplaceAll(link, "|", "\\|"))
					linked = true
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
	return b.String(), linked
}

// scenarioExistingReferenceSpan returns the length of a Markdown construct at
// the start of rest that is already a complete reference and is never
// rewritten: an inline link or image, an autolink, or a bare URL. It returns
// 0 when none starts here.
func scenarioExistingReferenceSpan(rest string) int {
	text := rest
	if strings.HasPrefix(text, "![") {
		text = text[1:]
	}
	if strings.HasPrefix(text, "[") {
		if close := scenarioBracketSpan(text); close > 0 && close < len(text) && text[close] == '(' {
			if n := scenarioInlineLinkSuffixSpan(text[close+1:]); n > 0 {
				return len(rest) - len(text) + close + 1 + n
			}
		}
	}
	if strings.HasPrefix(rest, "<") {
		if end := strings.IndexByte(rest, '>'); end > 0 && scenarioStartsWithURL(rest[1:end]) {
			return end + 1
		}
	}
	if scenarioStartsWithURL(rest) {
		if end := strings.IndexFunc(rest, unicode.IsSpace); end >= 0 {
			return end
		}
		return len(rest)
	}
	return 0
}

// scenarioBracketSpan returns the index just past the "]" that closes the "["
// at the start of text, honoring nested brackets (a clickable image's own
// text) and backslash escapes, or 0 when it is never closed.
func scenarioBracketSpan(text string) int {
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// scenarioCodeSpan returns the content and the total length of the inline
// code span at the start of rest - a backtick run closed by a run of the same
// length - or n == 0 when rest does not start with a closed span.
func scenarioCodeSpan(rest string) (content string, n int) {
	if !strings.HasPrefix(rest, "`") {
		return "", 0
	}
	run := 0
	for run < len(rest) && rest[run] == '`' {
		run++
	}
	for i := run; i < len(rest); {
		if rest[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(rest) && rest[j] == '`' {
			j++
		}
		if j-i == run {
			return rest[run:i], j
		}
		i = j
	}
	return "", 0
}

func scenarioInlineLinkSuffixSpan(text string) int {
	i := 0
	space := func() {
		for i < len(text) && strings.ContainsRune(" \t\r\n", rune(text[i])) {
			i++
		}
	}
	space()
	if i < len(text) && text[i] == '<' {
		i++
		for i < len(text) && text[i] != '>' {
			if text[i] == '<' || text[i] == '\n' || text[i] == '\r' {
				return 0
			}
			if text[i] == '\\' && i+1 < len(text) {
				i++
			}
			i++
		}
		if i == len(text) {
			return 0
		}
		i++
	} else {
		depth := 0
		for i < len(text) {
			c := text[i]
			if c == '\\' && i+1 < len(text) {
				i += 2
				continue
			}
			if strings.ContainsRune(" \t\r\n", rune(c)) || (c == ')' && depth == 0) {
				break
			}
			if c == '(' {
				depth++
			} else if c == ')' {
				depth--
			}
			i++
		}
		if depth != 0 {
			return 0
		}
	}
	endDestination := i
	space()
	if i > endDestination && i < len(text) && strings.ContainsRune("\"'(", rune(text[i])) {
		close := text[i]
		if close == '(' {
			close = ')'
		}
		i++
		for i < len(text) && text[i] != close {
			if close == ')' && text[i] == '(' {
				return 0
			}
			if text[i] == '\\' && i+1 < len(text) {
				i++
			}
			i++
		}
		if i == len(text) {
			return 0
		}
		i++
		space()
	}
	if i < len(text) && text[i] == ')' {
		return i + 1
	}
	return 0
}

// scenarioStartsWithURL reports whether s begins with a scheme followed by
// "://", the shape of a bare URL or an autolink body.
func scenarioStartsWithURL(s string) bool {
	i := strings.Index(s, "://")
	if i <= 0 || i > 16 {
		return false
	}
	for _, r := range s[:i] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// matchScenarioEvidenceReference returns the file reference at the start of
// rest - the longest existing relative name, else an absolute path inside the
// evidence root - as its link label, its filesystem path, and the number of
// bytes it spans; n is 0 when rest starts with no file reference.
func matchScenarioEvidenceReference(rest, root string, names []string) (label, path string, n int) {
	reference := rest
	var quote byte
	if len(reference) > 0 && strings.ContainsRune("\"'", rune(reference[0])) {
		quote = reference[0]
		reference = reference[1:]
	}
	offsets := []int{0}
	if absoluteRoot, err := filepath.Abs(root); err == nil {
		for _, dir := range []string{absoluteRoot, resolveArtifactPathSymlinks(absoluteRoot)} {
			prefix := strings.TrimRight(dir, string(filepath.Separator)) + string(filepath.Separator)
			if strings.HasPrefix(reference, prefix) {
				offsets = append(offsets, len(prefix))
			}
		}
	}
	for _, name := range names {
		for _, offset := range offsets {
			candidate := name
			if offset > 0 {
				candidate = filepath.FromSlash(name)
			}
			if !strings.HasPrefix(reference[offset:], candidate) {
				continue
			}
			end := offset + len(candidate)
			if quote != 0 {
				if end >= len(reference) || reference[end] != quote {
					continue
				}
				end++
			}
			if scenarioReferenceCanEnd(reference[end:]) {
				if quote != 0 {
					end++
				}
				return name, filepath.Join(root, filepath.FromSlash(name)), end
			}
		}
	}
	return "", "", 0
}

// scenarioReferenceCanStart reports whether a file reference may begin at
// byte offset pos of text: at the start, or after whitespace or a separator.
func scenarioReferenceCanStart(text string, pos int) bool {
	if pos == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:pos])
	return unicode.IsSpace(r) || strings.ContainsRune(";,([|`\"'", r)
}

// scenarioReferenceCanEnd reports whether a file reference that ends where
// after begins is complete: the cell ends, a separator or whitespace follows,
// or a sentence-ending period does.
func scenarioReferenceCanEnd(after string) bool {
	if after == "" {
		return true
	}
	r, size := utf8.DecodeRuneInString(after)
	if unicode.IsSpace(r) || strings.ContainsRune(";,)]|`\"'", r) {
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

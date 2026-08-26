package steps

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	maxPRIntentRunes            = 360
	maxPRAcSummaryRunes         = 100
	maxPRAcDetailRunes          = 600
	maxPRAcCount                = 7
	maxPRWhatChangedBulletRunes = 180
	maxPRWhatChangedBullets     = 3
	maxPRRiskVisibleRunes       = 240
	maxPRRiskDetailRunes        = 600
)

type prAcceptanceCriterion struct {
	Summary string `json:"summary"`
	Details string `json:"details"`
}

// decodePRContent decodes fields independently so a malformed optional
// overview cannot discard a valid legacy title/body response.
func decodePRContent(raw json.RawMessage) (prContent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return prContent{}, err
	}
	var content prContent
	decodeStringField(fields["title"], &content.Title)
	decodeStringField(fields["body"], &content.Body)
	decodeStringField(fields["intent"], &content.Intent)
	if value := fields["acceptance_criteria"]; len(value) > 0 && string(value) != "null" {
		var items []json.RawMessage
		if json.Unmarshal(value, &items) == nil {
			for _, item := range items {
				var itemFields map[string]json.RawMessage
				if json.Unmarshal(item, &itemFields) != nil {
					continue
				}
				var criterion prAcceptanceCriterion
				decodeStringField(itemFields["summary"], &criterion.Summary)
				decodeStringField(itemFields["details"], &criterion.Details)
				if strings.TrimSpace(criterion.Summary) != "" {
					content.AcceptanceCriteria = append(content.AcceptanceCriteria, criterion)
				}
			}
		}
	}
	return content, nil
}

func decodeStringField(raw json.RawMessage, target *string) {
	if target == nil || len(raw) == 0 || string(raw) == "null" {
		return
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		*target = value
	}
}

func renderConcisePRNarrative(content prContent, sctx *pipeline.StepContext, provider scm.Provider, finalDiff string) string {
	sourceIntent := neutralizeAttestationMarkers(cleanedUserIntent(sctx))
	overview := renderConcisePROverview(content, sourceIntent, intentSourceIsAuthoritative(sctx), prBodyFlavorFor(provider))
	whatChanged := normalizeWhatChanged(content.Body, finalDiff, prBodyFlavorFor(provider))
	if overview == "" {
		return whatChanged
	}
	return overview + "\n\n" + whatChanged
}

func renderConcisePROverview(content prContent, sourceIntent string, authoritative bool, flavor prBodyFlavor) string {
	if sourceIntent == "" {
		return ""
	}
	intentText := boundedHumanText(content.Intent, maxPRIntentRunes, 2)
	criteria := normalizeAcceptanceCriteria(content.AcceptanceCriteria)
	if intentText == "" || len(criteria) == 0 {
		fallbackIntent, fallbackCriteria := fallbackOverviewFromIntent(sourceIntent)
		if intentText == "" {
			intentText = fallbackIntent
		}
		if len(criteria) == 0 {
			criteria = fallbackCriteria
		}
	}
	if intentText == "" {
		intentText = boundedHumanText(sourceIntent, maxPRIntentRunes, 2)
	}
	if len(criteria) == 0 {
		criteria = []prAcceptanceCriterion{{
			Summary: "The requested outcome and constraints are satisfied",
			Details: boundedHumanText(sourceIntent, maxPRAcDetailRunes, 0),
		}}
	}

	var b strings.Builder
	b.WriteString("## Intent\n\n")
	b.WriteString(encodePRText(intentText, flavor))
	b.WriteString("\n\n")
	if flavor == prBodyMarkdown {
		b.WriteString("### Acceptance criteria\n\n")
		for i, criterion := range criteria {
			b.WriteString(renderBitbucketAcceptanceCriterion(i+1, criterion))
			b.WriteString("\n")
		}
		b.WriteString("\n### Complete acceptance context\n\n")
		if !authoritative {
			b.WriteString("_Inferred context:_ ")
		}
		b.WriteString(encodePRText(sourceIntent, flavor))
		return strings.TrimSpace(b.String())
	}

	b.WriteString("<details>\n")
	b.WriteString(fmt.Sprintf("<summary><strong>Acceptance criteria</strong> — %d brief check%s</summary>\n\n", len(criteria), pluralSuffix(len(criteria))))
	for i, criterion := range criteria {
		b.WriteString("<details>\n")
		b.WriteString(fmt.Sprintf("<summary><strong>AC%d</strong> — %s</summary>\n\n", i+1, encodePRText(criterion.Summary, flavor)))
		b.WriteString(encodePRText(criterion.Details, flavor))
		b.WriteString("\n\n</details>\n\n")
	}
	b.WriteString("<details>\n")
	b.WriteString("<summary><strong>Complete acceptance context</strong></summary>\n\n")
	if !authoritative {
		b.WriteString("<em>Inferred context:</em> ")
	}
	b.WriteString(encodePRText(sourceIntent, flavor))
	b.WriteString("\n\n</details>\n\n</details>")
	return strings.TrimSpace(b.String())
}

func normalizeAcceptanceCriteria(items []prAcceptanceCriterion) []prAcceptanceCriterion {
	criteria := make([]prAcceptanceCriterion, 0, min(len(items), maxPRAcCount))
	for _, item := range items {
		summary := boundedHumanText(item.Summary, maxPRAcSummaryRunes, 1)
		if summary == "" {
			continue
		}
		details := boundedHumanText(item.Details, maxPRAcDetailRunes, 0)
		if details == "" {
			details = summary
		}
		criteria = append(criteria, prAcceptanceCriterion{Summary: summary, Details: details})
		if len(criteria) == maxPRAcCount {
			break
		}
	}
	return criteria
}

func fallbackOverviewFromIntent(source string) (string, []prAcceptanceCriterion) {
	lines := strings.Split(source, "\n")
	marker := -1
	for i, line := range lines {
		normalized := strings.ToLower(strings.Trim(strings.TrimSpace(line), "#*: "))
		if normalized == "acceptance criteria" {
			marker = i
			break
		}
	}
	intentPart := source
	if marker >= 0 {
		intentPart = strings.Join(lines[:marker], "\n")
	}
	intentText := boundedHumanText(intentPart, maxPRIntentRunes, 2)
	var criteria []prAcceptanceCriterion
	if marker >= 0 {
		for _, line := range lines[marker+1:] {
			criterion, ok := parseAcceptanceCriterionLine(line)
			if !ok {
				continue
			}
			criteria = append(criteria, criterion)
			if len(criteria) == maxPRAcCount {
				break
			}
		}
	}
	return intentText, criteria
}

func parseAcceptanceCriterionLine(line string) (prAcceptanceCriterion, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		line = strings.TrimSpace(line[2:])
	} else {
		return prAcceptanceCriterion{}, false
	}
	for _, checkbox := range []string{"[ ]", "[x]", "[X]"} {
		if strings.HasPrefix(line, checkbox) {
			line = strings.TrimSpace(strings.TrimPrefix(line, checkbox))
			break
		}
	}
	line = strings.ReplaceAll(line, "**", "")
	upper := strings.ToUpper(line)
	if !strings.HasPrefix(upper, "AC") {
		return prAcceptanceCriterion{}, false
	}
	line = trimAcceptanceCriterionLabel(line)
	summary, details := line, line
	if idx := strings.Index(line, ":"); idx > 0 {
		summary = strings.TrimSpace(line[:idx])
		details = strings.TrimSpace(line[idx+1:])
	}
	summary = boundedHumanText(summary, maxPRAcSummaryRunes, 1)
	details = boundedHumanText(details, maxPRAcDetailRunes, 0)
	if summary == "" {
		return prAcceptanceCriterion{}, false
	}
	if details == "" {
		details = summary
	}
	return prAcceptanceCriterion{Summary: summary, Details: details}, true
}

// trimAcceptanceCriterionLabel removes a leading "ACn" label and the one
// delimiter that immediately follows it. The delimiter is only recognized in
// that anchored position: a hyphen or dash anywhere else in the line belongs to
// the criterion's own words ("non-blocking"), not to its label.
func trimAcceptanceCriterionLabel(line string) string {
	rest := line[len("AC"):]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return line
	}
	rest = strings.TrimSpace(rest[digits:])
	for _, delimiter := range []string{"—", "–", "-", ":", ".", ")"} {
		if strings.HasPrefix(rest, delimiter) {
			return strings.TrimSpace(strings.TrimPrefix(rest, delimiter))
		}
	}
	return rest
}

func normalizeWhatChanged(body, finalDiff string, flavor prBodyFlavor) string {
	body = stripGeneratedSections(unwrapNestedPRBody(strings.TrimSpace(body)))
	lines := strings.Split(body, "\n")
	sectionStart, sectionEnd := whatChangedSectionBounds(lines)
	var bullets, extra []string
	for _, raw := range lines[sectionStart:sectionEnd] {
		rawLine := strings.TrimRight(raw, " \t\r")
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "## ") {
			continue
		}
		if strings.HasPrefix(rawLine, "- ") || strings.HasPrefix(rawLine, "* ") {
			if len(bullets) < maxPRWhatChangedBullets {
				if text := boundedHumanText(rawLine[2:], maxPRWhatChangedBulletRunes, 0); text != "" {
					bullets = append(bullets, text)
				}
			}
			continue
		}
		extra = append(extra, line)
	}
	fallbackDetail := boundedMultilineText(strings.Join(extra, "\n"), 1200)
	if len(bullets) == 0 {
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(finalDiff), "\n") {
			if strings.TrimSpace(line) != "" {
				count++
			}
		}
		if count == 0 {
			bullets = []string{"Updated the final branch delta."}
		} else {
			bullets = []string{fmt.Sprintf("Updated %d file%s in the final branch delta.", count, pluralSuffix(count))}
			diffDetail := boundedMultilineText(finalDiff, 1200)
			if fallbackDetail == "" {
				fallbackDetail = diffDetail
			} else if diffDetail != "" {
				fallbackDetail += "\n\n" + diffDetail
			}
		}
	}
	var b strings.Builder
	b.WriteString("## What Changed\n\n")
	for _, bullet := range bullets {
		b.WriteString("- ")
		b.WriteString(encodePRText(bullet, flavor))
		b.WriteString("\n")
	}
	if fallbackDetail != "" {
		renderWhatChangedDetail(&b, fallbackDetail, flavor)
	}
	return strings.TrimSpace(b.String())
}

func whatChangedSectionBounds(lines []string) (start, end int) {
	start, end = 0, len(lines)
	firstHeading := -1
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		if firstHeading < 0 {
			firstHeading = i
		}
		heading := strings.ToLower(strings.Trim(strings.TrimPrefix(line, "## "), ":.!? "))
		if heading == "what changed" {
			start = i + 1
			for j := start; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "## ") {
					return start, j
				}
			}
			return start, len(lines)
		}
	}
	if firstHeading >= 0 {
		start = firstHeading + 1
		for j := start; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "## ") {
				return start, j
			}
		}
	}
	return start, end
}

func renderWhatChangedDetail(b *strings.Builder, detail string, flavor prBodyFlavor) {
	if flavor == prBodyMarkdown {
		b.WriteString("\n### Additional change detail\n\n```text\n")
		b.WriteString(escapeMarkdownFence(escapePipelineFoldMarkers(detail)))
		b.WriteString("\n```\n")
		return
	}
	b.WriteString("\n<details>\n<summary>Additional change detail</summary>\n\n<pre>")
	b.WriteString(html.EscapeString(detail))
	b.WriteString("</pre>\n\n</details>\n")
}

func renderConciseRisk(risk string, provider scm.Provider) string {
	risk = strings.TrimSpace(neutralizeAttestationMarkers(risk))
	if risk == "" {
		return ""
	}
	visible := boundedHumanText(risk, maxPRRiskVisibleRunes, 1)
	if visible == boundedHumanText(risk, maxPRRiskDetailRunes, 0) {
		return encodePRText(visible, prBodyFlavorFor(provider))
	}
	detail := boundedHumanText(risk, maxPRRiskDetailRunes, 0)
	flavor := prBodyFlavorFor(provider)
	if flavor == prBodyMarkdown {
		remainder := riskDetailBeyondVisible(visible, detail)
		if remainder == "" {
			return encodePRText(visible, flavor)
		}
		return encodePRText(visible, flavor) + "\n\n  " + encodePRText(remainder, flavor)
	}
	return encodePRText(visible, flavor) + "\n\n<details>\n<summary>More risk detail</summary>\n\n" + encodePRText(detail, flavor) + "\n\n</details>"
}

// riskDetailBeyondVisible returns the part of the bounded rationale the visible
// sentence does not already show. Without a disclosure to hide it, a flavor
// that renders the detail in the open would otherwise repeat that sentence.
func riskDetailBeyondVisible(visible, detail string) string {
	if strings.HasPrefix(detail, visible) {
		return strings.TrimSpace(detail[len(visible):])
	}
	if shown := strings.TrimSuffix(visible, "…"); shown != visible && strings.HasPrefix(detail, shown) {
		return strings.TrimSpace(detail[len(shown):])
	}
	return detail
}

func boundedMultilineText(text string, maxRunes int) string {
	text = strings.TrimSpace(neutralizeAttestationMarkers(text))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return strings.TrimSpace(string(runes[:maxRunes-1])) + "…"
}

func boundedHumanText(text string, maxRunes, maxSentences int) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(neutralizeAttestationMarkers(text))), " ")
	if text == "" {
		return ""
	}
	if maxSentences > 0 {
		text = firstSentences(text, maxSentences)
	}
	return truncateAtWordBoundary(text, maxRunes)
}

func firstSentences(text string, max int) string {
	if max <= 0 {
		return text
	}
	count := 0
	for i, r := range text {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		next := i + utf8.RuneLen(r)
		if next < len(text) {
			nextRune, _ := utf8.DecodeRuneInString(text[next:])
			if !unicode.IsSpace(nextRune) {
				continue
			}
		}
		count++
		if count == max {
			return strings.TrimSpace(text[:next])
		}
	}
	return text
}

func truncateAtWordBoundary(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	limit := maxRunes - 1
	if limit <= 0 {
		return "…"
	}
	cut := limit
	for cut > 0 && !unicode.IsSpace(runes[cut-1]) {
		cut--
	}
	if cut == 0 {
		cut = limit
	}
	return strings.TrimSpace(string(runes[:cut])) + "…"
}

func encodePRText(text string, flavor prBodyFlavor) string {
	text = neutralizeAttestationMarkers(strings.TrimSpace(text))
	if flavor == prBodyMarkdown {
		text = escapePipelineFoldMarkers(escapeMarkdownFence(text))
		text = strings.ReplaceAll(text, "\\", "\\\\")
		text = escapeMarkdownStructuralLines(text)
		return escapeMarkdownMetacharacters(text, "\\")
	}
	const hashSentinel = "\ue000"
	text = strings.ReplaceAll(text, "#", hashSentinel)
	text = html.EscapeString(text)
	text = strings.ReplaceAll(text, hashSentinel, "&#35;")
	text = escapeMarkdownMetacharacters(text, "entity")
	return escapeHTMLMarkdownStructuralLines(text)
}

func renderBitbucketAcceptanceCriterion(index int, criterion prAcceptanceCriterion) string {
	const maxBulletRunes = 300
	prefix := fmt.Sprintf("- **AC%d — ", index)
	suffix := ":** "
	summary := boundedEncodedPRText(criterion.Summary, maxPRAcSummaryRunes, prBodyMarkdown)
	detailBudget := max(1, maxBulletRunes-len([]rune(prefix+summary+suffix)))
	details := boundedEncodedPRText(criterion.Details, detailBudget, prBodyMarkdown)
	line := prefix + summary + suffix + details
	for len([]rune(line)) > maxBulletRunes && detailBudget > 1 {
		detailBudget -= len([]rune(line)) - maxBulletRunes
		details = boundedEncodedPRText(criterion.Details, max(1, detailBudget), prBodyMarkdown)
		line = prefix + summary + suffix + details
	}
	return line
}

func boundedEncodedPRText(text string, maxRunes int, flavor prBodyFlavor) string {
	limit := min(len([]rune(text)), maxRunes)
	for limit > 0 {
		encoded := encodePRText(truncateAtWordBoundary(strings.Join(strings.Fields(text), " "), limit), flavor)
		if len([]rune(encoded)) <= maxRunes {
			return encoded
		}
		limit -= len([]rune(encoded)) - maxRunes
	}
	return ""
}

func escapeMarkdownMetacharacters(text, mode string) string {
	characters := []string{"!", "[", "]", "<", ">", "*", "_", "`", "{", "}", "|", "~", "$"}
	for _, character := range characters {
		replacement := "\\" + character
		if mode == "entity" {
			replacement = fmt.Sprintf("&#%d;", []rune(character)[0])
		}
		text = strings.ReplaceAll(text, character, replacement)
	}
	if mode == "entity" {
		text = strings.ReplaceAll(text, "\\", "&#92;")
	}
	return text
}

func escapeMarkdownStructuralLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := line[:len(line)-len(trimmed)]
		if isMarkdownStructuralLine(trimmed) {
			lines[i] = indent + "\\" + trimmed
		}
	}
	return strings.Join(lines, "\n")
}

func escapeHTMLMarkdownStructuralLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := line[:len(line)-len(trimmed)]
		if isMarkdownStructuralLine(trimmed) {
			lines[i] = indent + "&#8203;" + trimmed
		}
	}
	return strings.Join(lines, "\n")
}

func isMarkdownStructuralLine(trimmed string) bool {
	setext := len(trimmed) >= 3 && (strings.Trim(trimmed, "=") == "" || strings.Trim(trimmed, "-") == "")
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") || setext ||
		strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ ") ||
		strings.HasPrefix(trimmed, "> ") || isOrderedMarkdownListLine(trimmed)
}

func isOrderedMarkdownListLine(text string) bool {
	index := 0
	for index < len(text) && text[index] >= '0' && text[index] <= '9' {
		index++
	}
	if index == 0 || index+1 >= len(text) {
		return false
	}
	return (text[index] == '.' || text[index] == ')') && text[index+1] == ' '
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

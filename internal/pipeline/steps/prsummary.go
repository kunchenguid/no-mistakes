package steps

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BuildPipelineSummary produces a deterministic markdown section from step results and rounds.
func BuildPipelineSummary(steps []*db.StepResult, rounds map[string][]*db.StepRound) (string, string) {
	if len(steps) == 0 {
		return "", ""
	}

	var detailBlocks []string

	for _, sr := range steps {
		if shouldOmitPipelineStep(sr) {
			continue
		}
		stepRounds := rounds[sr.ID]
		line, detail := buildStepEntry(sr, stepRounds)
		if line != "" && detail != "" {
			detailBlocks = append(detailBlocks, detail)
		}
	}

	if len(detailBlocks) == 0 {
		return "", ""
	}

	var b strings.Builder
	b.WriteString("## Pipeline\n\nUpdates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)\n\n")
	for i, detail := range detailBlocks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(detail)
	}

	riskLine := extractRiskLine(steps, rounds)
	return b.String(), riskLine
}

// BuildTestingSummary extracts a deterministic Testing section from the test step.
func BuildTestingSummary(steps []*db.StepResult, rounds map[string][]*db.StepRound) string {
	for _, sr := range steps {
		if sr.StepName != types.StepTest {
			continue
		}

		stepRounds := rounds[sr.ID]
		line, _ := buildStepEntry(sr, stepRounds)
		if line == "" {
			return ""
		}

		testingSummary := collectTestingSummary(sr, stepRounds)
		tested := collectTestingDetails(sr, stepRounds)
		if testingSummary == "" && len(tested) == 0 {
			return "## Testing\n\n- " + line
		}

		var b strings.Builder
		b.WriteString("## Testing\n\n")
		if testingSummary != "" {
			b.WriteString("- Summary: ")
			b.WriteString(testingSummary)
			b.WriteString("\n")
		}
		for _, detail := range tested {
			b.WriteString("- ")
			b.WriteString(detail)
			b.WriteString("\n")
		}
		if outcome := buildTestingOutcomeLine(line, stepRounds); outcome != "" {
			b.WriteString("- ")
			b.WriteString(outcome)
			b.WriteString("\n")
		}

		return strings.TrimSpace(b.String())
	}

	return ""
}

func collectTestingSummary(sr *db.StepResult, rounds []*db.StepRound) string {
	if summary := testingSummaryFromFindings(sr.FindingsJSON); summary != "" {
		return summary
	}
	for i := len(rounds) - 1; i >= 0; i-- {
		if summary := testingSummaryFromFindings(rounds[i].FindingsJSON); summary != "" {
			return summary
		}
	}
	return ""
}

func testingSummaryFromFindings(raw *string) string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return ""
	}
	return sanitizePromptMultilineText(findings.TestingSummary)
}

func collectTestingDetails(sr *db.StepResult, rounds []*db.StepRound) []string {
	seen := map[string]bool{}
	details := appendTestingDetails(nil, seen, sr.FindingsJSON)
	for _, r := range rounds {
		details = appendTestingDetails(details, seen, r.FindingsJSON)
	}
	return details
}

func appendTestingDetails(details []string, seen map[string]bool, raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return details
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return details
	}
	for _, detail := range findings.Tested {
		clean := sanitizePromptText(detail)
		if clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		details = append(details, clean)
	}
	return details
}

func buildTestingOutcomeLine(summaryLine string, rounds []*db.StepRound) string {
	outcome := strings.TrimSpace(strings.Replace(summaryLine, "**Test** - ", "", 1))
	if outcome == "" {
		return ""
	}
	if len(rounds) == 0 {
		return "Outcome: " + outcome
	}
	runLabel := "1 run"
	if len(rounds) != 1 {
		runLabel = fmt.Sprintf("%d runs", len(rounds))
	}
	totalDuration := int64(0)
	for _, r := range rounds {
		totalDuration += r.DurationMS
	}
	if totalDuration > 0 {
		return fmt.Sprintf("Outcome: %s across %s (%s)", outcome, runLabel, formatTestingDuration(totalDuration))
	}
	return fmt.Sprintf("Outcome: %s across %s", outcome, runLabel)
}

func formatTestingDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		seconds := float64(ms) / 1000
		if ms%1000 == 0 {
			return fmt.Sprintf("%ds", ms/1000)
		}
		return fmt.Sprintf("%.1fs", seconds)
	}
	return d.Round(time.Second).String()
}

func buildStepEntry(sr *db.StepResult, rounds []*db.StepRound) (statusLine, detailBlock string) {
	name := stepDisplayName(sr.StepName)
	buildDetail := func(line string) (string, string) {
		return line, buildStepDetails(line, sr, rounds)
	}

	switch sr.Status {
	case types.StepStatusPending:
		return buildDetail(fmt.Sprintf("⏳ **%s** - pending", name))
	case types.StepStatusRunning:
		return buildDetail(fmt.Sprintf("⏳ **%s** - running", name))
	case types.StepStatusAwaitingApproval:
		return buildDetail(fmt.Sprintf("⏸️ **%s** - awaiting approval", name))
	case types.StepStatusFixing:
		return buildDetail(fmt.Sprintf("🔄 **%s** - auto-fixing", name))
	case types.StepStatusFixReview:
		return buildDetail(fmt.Sprintf("⏸️ **%s** - review fix", name))
	case types.StepStatusFailed:
		return buildDetail(fmt.Sprintf("❌ **%s** - failed", name))
	}

	if sr.Status == types.StepStatusSkipped {
		return buildDetail(fmt.Sprintf("⏭️ **%s** - skipped", name))
	}

	// Parse the final findings on the step result (last state).
	var finalFindings *types.Findings
	finalFindingsParsed := sr.FindingsJSON == nil
	if sr.FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*sr.FindingsJSON); err == nil {
			finalFindings = &f
			finalFindingsParsed = true
		}
	}

	// Parse initial round findings (round 1) for the full story.
	var initialFindings *types.Findings
	if len(rounds) > 0 && rounds[0].FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*rounds[0].FindingsJSON); err == nil {
			initialFindings = &f
		}
	}

	// Parse latest round findings for risk fallback when final state is cleared.
	var latestRoundFindings *types.Findings
	if len(rounds) > 0 {
		last := rounds[len(rounds)-1]
		if last.FindingsJSON != nil {
			if f, err := types.ParseFindingsJSON(*last.FindingsJSON); err == nil {
				latestRoundFindings = &f
			}
		}
	}

	hadFindings := initialFindings != nil && len(initialFindings.Items) > 0
	hasFinalFindings := finalFindings != nil && len(finalFindings.Items) > 0
	hasAnyRoundFindings := roundsHaveFindings(rounds)
	hasRoundParseFailure := roundsHaveParseFailure(rounds)
	hadAnyFindings := hadFindings || hasFinalFindings || hasAnyRoundFindings
	hasUnreadableFinalFindings := sr.FindingsJSON != nil && !finalFindingsParsed
	wasFixed := hadFindings && len(rounds) > 1 && !hasUnreadableFinalFindings && !hasFinalFindings
	riskLevel := ""
	if sr.StepName == types.StepReview {
		src := finalFindings
		if src == nil && !hasUnreadableFinalFindings {
			src = latestRoundFindings
		}
		if src != nil {
			riskLevel = src.RiskLevel
		}
	}

	// Unreadable final findings - can't make claims about the outcome.
	if hasUnreadableFinalFindings {
		return buildDetail(fmt.Sprintf("⚠️ **%s** - findings unavailable", name))
	}

	if sr.StepName == types.StepReview && (riskLevel == "medium" || riskLevel == "high") && !hadAnyFindings {
		return buildDetail(fmt.Sprintf("%s **%s** - %s risk", riskEmoji(riskLevel), name, riskLevel))
	}

	if !hadAnyFindings && !hasRoundParseFailure {
		if len(rounds) == 0 {
			return buildDetail(fmt.Sprintf("⚠️ **%s** - findings unavailable", name))
		}
		return buildDetail(fmt.Sprintf("✅ **%s** - passed", name))
	}

	if hasRoundParseFailure && !hadAnyFindings {
		return buildDetail(fmt.Sprintf("⚠️ **%s** - findings unavailable", name))
	}

	if wasFixed {
		result := buildFixResultText(rounds)
		line := fmt.Sprintf("🔧 **%s** - %s", name, result)
		return buildDetail(line)
	}

	currentFindings := initialFindings
	if hasFinalFindings {
		currentFindings = finalFindings
	}

	// Had findings and the final state still contains them - approved as-is.
	count := countFindingsBySeverity(currentFindings)
	line := fmt.Sprintf("⚠️ **%s** - %s", name, count)
	return buildDetail(line)
}

func extractRiskLine(steps []*db.StepResult, rounds map[string][]*db.StepRound) string {
	for _, sr := range steps {
		if sr.StepName != types.StepReview {
			continue
		}

		var finalFindings *types.Findings
		hasUnreadableFinal := false
		if sr.FindingsJSON != nil {
			if f, err := types.ParseFindingsJSON(*sr.FindingsJSON); err == nil {
				finalFindings = &f
			} else {
				hasUnreadableFinal = true
			}
		}

		src := finalFindings
		if src == nil && !hasUnreadableFinal {
			stepRounds := rounds[sr.ID]
			if len(stepRounds) > 0 {
				last := stepRounds[len(stepRounds)-1]
				if last.FindingsJSON != nil {
					if f, err := types.ParseFindingsJSON(*last.FindingsJSON); err == nil {
						src = &f
					}
				}
			}
		}

		if src == nil || src.RiskLevel == "" {
			return ""
		}

		emoji := riskEmoji(src.RiskLevel)
		label := capitalizeRisk(src.RiskLevel)
		if src.RiskRationale != "" {
			return fmt.Sprintf("%s %s: %s", emoji, label, src.RiskRationale)
		}
		return fmt.Sprintf("%s %s", emoji, label)
	}
	return ""
}

func capitalizeRisk(level string) string {
	if level == "" {
		return level
	}
	return strings.ToUpper(level[:1]) + level[1:]
}

func riskEmoji(level string) string {
	switch level {
	case "low":
		return "✅"
	case "medium":
		return "⚠️"
	case "high":
		return "🚨"
	default:
		return "ℹ️"
	}
}

func roundsHaveFindings(rounds []*db.StepRound) bool {
	for _, r := range rounds {
		if r.FindingsJSON == nil {
			continue
		}
		f, err := types.ParseFindingsJSON(*r.FindingsJSON)
		if err != nil {
			continue
		}
		if len(f.Items) > 0 {
			return true
		}
	}

	return false
}

func roundsHaveParseFailure(rounds []*db.StepRound) bool {
	for _, r := range rounds {
		if r.FindingsJSON == nil {
			continue
		}
		if _, err := types.ParseFindingsJSON(*r.FindingsJSON); err != nil {
			return true
		}
	}

	return false
}

func buildFixResultText(rounds []*db.StepRound) string {
	// Count findings in round 1.
	var initialCount int
	if len(rounds) > 0 && rounds[0].FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*rounds[0].FindingsJSON); err == nil {
			initialCount = len(f.Items)
		}
	}

	// Categorize fix rounds. Legacy "user_fix" rounds are rendered as auto-fix.
	autoFixRounds := 0
	for _, r := range rounds[1:] {
		switch r.Trigger {
		case "auto_fix":
			autoFixRounds++
		case "user_fix":
			autoFixRounds++
		}
	}

	noun := "issue"
	if initialCount != 1 {
		noun = "issues"
	}

	parts := []string{fmt.Sprintf("%d %s found", initialCount, noun)}

	if autoFixRounds > 1 {
		parts = append(parts, fmt.Sprintf("auto-fixed (%d)", autoFixRounds))
	} else if autoFixRounds == 1 {
		parts = append(parts, "auto-fixed")
	}

	return strings.Join(parts, " → ")
}

func buildStepDetails(summaryLine string, sr *db.StepResult, rounds []*db.StepRound) string {
	var b strings.Builder
	b.WriteString("<details>\n")
	b.WriteString(fmt.Sprintf("<summary>%s</summary>\n\n", summaryLine))

	if len(rounds) == 0 {
		writeStepStatusDetail(&b, sr)
		b.WriteString("</details>\n")
		return b.String()
	}

	missingRoundFindingsData := sr.FindingsJSON != nil && !roundsHaveFindings(rounds) && !roundsHaveParseFailure(rounds)

	for _, r := range rounds {
		triggerLabel := ""
		switch r.Trigger {
		case "initial":
			triggerLabel = ""
		case "auto_fix":
			triggerLabel = " (auto-fix)"
		case "user_fix":
			triggerLabel = " (auto-fix)"
		}

		if r.FindingsJSON == nil {
			if missingRoundFindingsData {
				b.WriteString(fmt.Sprintf("**Round %d**%s - findings not recorded\n\n", r.Round, triggerLabel))
				continue
			}
			b.WriteString(fmt.Sprintf("**Round %d**%s - passed ✅\n\n", r.Round, triggerLabel))
			continue
		}

		findings, err := types.ParseFindingsJSON(*r.FindingsJSON)
		if err != nil {
			b.WriteString(fmt.Sprintf("**Round %d**%s - failed to parse findings\n\n", r.Round, triggerLabel))
			continue
		}
		if len(findings.Items) == 0 {
			b.WriteString(fmt.Sprintf("**Round %d**%s - passed ✅\n", r.Round, triggerLabel))
			if sr.StepName == types.StepTest {
				for _, detail := range findings.Tested {
					clean := strings.TrimSpace(detail)
					if clean == "" {
						continue
					}
					b.WriteString(fmt.Sprintf("- %s\n", html.EscapeString(clean)))
				}
			}
			b.WriteString("\n")
			continue
		}

		count := countFindingsBySeverity(&findings)
		b.WriteString(fmt.Sprintf("**Round %d**%s - found %s\n", r.Round, triggerLabel, count))

		for _, f := range findings.Items {
			emoji := severityEmoji(f.Severity)
			loc := ""
			if f.File != "" {
				loc = fmt.Sprintf("`%s", html.EscapeString(f.File))
				if f.Line > 0 {
					loc += fmt.Sprintf(":%d", f.Line)
				}
				loc += "` - "
			}
			b.WriteString(fmt.Sprintf("- %s %s%s\n", emoji, loc, html.EscapeString(f.Description)))
		}
		b.WriteString("\n")
	}

	b.WriteString("</details>\n")
	return b.String()
}

func writeStepStatusDetail(b *strings.Builder, sr *db.StepResult) {
	switch sr.Status {
	case types.StepStatusPending:
		b.WriteString("Step has not started yet.\n\n")
	case types.StepStatusRunning:
		b.WriteString("Step is currently running.\n\n")
	case types.StepStatusAwaitingApproval:
		b.WriteString("Waiting for user approval.\n\n")
	case types.StepStatusFixing:
		b.WriteString("Agent is currently applying fixes.\n\n")
	case types.StepStatusFixReview:
		b.WriteString("Waiting to review the latest fix.\n\n")
	case types.StepStatusSkipped:
		b.WriteString("Step was skipped.\n\n")
	case types.StepStatusFailed:
		if sr.Error != nil && strings.TrimSpace(*sr.Error) != "" {
			b.WriteString(html.EscapeString(strings.TrimSpace(*sr.Error)))
			b.WriteString("\n\n")
			return
		}
		b.WriteString("Step failed.\n\n")
	case types.StepStatusCompleted:
		b.WriteString("No round details recorded.\n\n")
	default:
		b.WriteString("Status unavailable.\n\n")
	}
}

func shouldOmitPipelineStep(sr *db.StepResult) bool {
	if sr == nil {
		return false
	}

	return sr.StepName == types.StepPR || sr.StepName == types.StepCI
}

func countFindingsBySeverity(findings *types.Findings) string {
	if findings == nil || len(findings.Items) == 0 {
		return "0 issues"
	}

	counts := map[string]int{}
	for _, f := range findings.Items {
		counts[f.Severity]++
	}

	total := len(findings.Items)
	noun := "issue"
	if total != 1 {
		noun = "issues"
	}

	// If all same severity, just show count + severity.
	if len(counts) == 1 {
		for sev, n := range counts {
			noun := sev
			if n != 1 {
				noun += "s"
			}
			return fmt.Sprintf("%d %s", n, noun)
		}
	}

	// Mixed severities: "3 issues (1 error, 2 warnings)"
	var parts []string
	for _, sev := range []string{"error", "warning", "info"} {
		if n, ok := counts[sev]; ok {
			label := sev
			if n != 1 {
				label += "s"
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	return fmt.Sprintf("%d %s (%s)", total, noun, strings.Join(parts, ", "))
}

func severityEmoji(severity string) string {
	switch severity {
	case "error":
		return "🚨"
	case "warning":
		return "⚠️"
	case "info":
		return "ℹ️"
	default:
		return "-"
	}
}

func stepDisplayName(name types.StepName) string {
	switch name {
	case types.StepRebase:
		return "Rebase"
	case types.StepReview:
		return "Review"
	case types.StepTest:
		return "Test"
	case types.StepDocument:
		return "Document"
	case types.StepLint:
		return "Lint"
	case types.StepPush:
		return "Push"
	case types.StepPR:
		return "PR"
	case types.StepCI:
		return "CI"
	default:
		return string(name)
	}
}

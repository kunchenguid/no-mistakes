package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// summarySteps are the steps we include in the pipeline summary.
// Push, PR, and Babysit are operational - not interesting for the PR reader.
var summarySteps = map[types.StepName]bool{
	types.StepRebase: true,
	types.StepReview: true,
	types.StepTest:   true,
	types.StepLint:   true,
}

// BuildPipelineSummary produces a deterministic markdown section from step results and rounds.
func BuildPipelineSummary(steps []*db.StepResult, rounds map[string][]*db.StepRound) string {
	if len(steps) == 0 {
		return ""
	}

	var statusLines []string
	var detailBlocks []string

	for _, sr := range steps {
		if !summarySteps[sr.StepName] {
			continue
		}
		stepRounds := rounds[sr.ID]
		line, detail := buildStepEntry(sr, stepRounds)
		if line != "" {
			statusLines = append(statusLines, line)
		}
		if detail != "" {
			detailBlocks = append(detailBlocks, detail)
		}
	}

	if len(statusLines) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Pipeline\n\n")
	for _, line := range statusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}

	for _, detail := range detailBlocks {
		b.WriteString("\n")
		b.WriteString(detail)
	}

	return b.String()
}

func buildStepEntry(sr *db.StepResult, rounds []*db.StepRound) (statusLine, detailBlock string) {
	name := stepDisplayName(sr.StepName)

	if sr.Status == types.StepStatusSkipped {
		return fmt.Sprintf("⏭️ **%s** - skipped", name), ""
	}

	// Parse the final findings on the step result (last state).
	var finalFindings *types.Findings
	if sr.FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*sr.FindingsJSON); err == nil {
			finalFindings = &f
		}
	}

	// Parse initial round findings (round 1) for the full story.
	var initialFindings *types.Findings
	if len(rounds) > 0 && rounds[0].FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*rounds[0].FindingsJSON); err == nil {
			initialFindings = &f
		}
	}

	hadFindings := initialFindings != nil && len(initialFindings.Items) > 0
	hasFinalFindings := finalFindings != nil && len(finalFindings.Items) > 0
	hasAnyRoundFindings := roundsHaveFindings(rounds)
	hasRoundParseFailure := roundsHaveParseFailure(rounds)
	hasRoundDetails := roundsNeedDetail(rounds)
	hadAnyFindings := hadFindings || hasFinalFindings || hasAnyRoundFindings
	wasFixed := hadFindings && len(rounds) > 1 && !hasFinalFindings

	// Special handling for review step - risk level is the primary signal.
	if sr.StepName == types.StepReview {
		return buildReviewEntry(name, finalFindings, initialFindings, rounds)
	}

	// For test/lint/rebase: determine emoji and result text.
	if !hadAnyFindings && !hasRoundParseFailure {
		detail := ""
		if hasRoundDetails {
			detail = buildRoundsDetail(name, rounds)
		}
		return fmt.Sprintf("✅ **%s** - passed", name), detail
	}

	if hasRoundParseFailure && !hadAnyFindings {
		detail := ""
		if hasRoundDetails {
			detail = buildRoundsDetail(name, rounds)
		}
		return fmt.Sprintf("⚠️ **%s** - findings unavailable", name), detail
	}

	if wasFixed {
		result := buildFixResultText(rounds)
		line := fmt.Sprintf("🔧 **%s** - %s", name, result)
		detail := buildRoundsDetail(name, rounds)
		return line, detail
	}

	currentFindings := initialFindings
	if hasFinalFindings {
		currentFindings = finalFindings
	}

	// Had findings and the final state still contains them - approved as-is.
	count := countFindingsBySeverity(currentFindings)
	line := fmt.Sprintf("⚠️ **%s** - %s", name, count)
	detail := ""
	if hasRoundDetails {
		detail = buildRoundsDetail(name, rounds)
	}
	return line, detail
}

func buildReviewEntry(name string, finalFindings, initialFindings *types.Findings, rounds []*db.StepRound) (string, string) {
	// Determine risk level and rationale from whichever findings have them.
	src := finalFindings
	if src == nil {
		src = initialFindings
	}

	riskLevel := ""
	rationale := ""
	if src != nil {
		riskLevel = src.RiskLevel
		rationale = src.RiskRationale
	}

	hasInitialFindings := initialFindings != nil && len(initialFindings.Items) > 0
	hasFinalFindings := finalFindings != nil && len(finalFindings.Items) > 0
	hasHistoricalFindings := hasInitialFindings || roundsNeedDetail(rounds)
	hasRoundParseFailure := roundsHaveParseFailure(rounds)
	emoji := "✅"
	if hasFinalFindings || hasRoundParseFailure || riskLevel == "medium" || riskLevel == "high" {
		emoji = "⚠️"
	}

	var line string
	if riskLevel != "" {
		line = fmt.Sprintf("%s **%s** - %s risk", emoji, name, riskLevel)
		if rationale != "" {
			line += fmt.Sprintf(` - _"%s"_`, rationale)
		}
	} else if hasRoundParseFailure {
		line = fmt.Sprintf("%s **%s** - findings unavailable", emoji, name)
	} else {
		line = fmt.Sprintf("%s **%s** - passed", emoji, name)
	}

	detail := ""
	if hasHistoricalFindings {
		detail = buildRoundsDetail(name, rounds)
	}
	return line, detail
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

func roundsNeedDetail(rounds []*db.StepRound) bool {
	for _, r := range rounds {
		if r.FindingsJSON == nil {
			continue
		}
		if _, err := types.ParseFindingsJSON(*r.FindingsJSON); err != nil {
			return true
		}
		f, err := types.ParseFindingsJSON(*r.FindingsJSON)
		if err != nil {
			return true
		}
		if len(f.Items) > 0 {
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

	// Categorize fix rounds.
	autoFixRounds := 0
	userFixRounds := 0
	for _, r := range rounds[1:] {
		switch r.Trigger {
		case "auto_fix":
			autoFixRounds++
		case "user_fix":
			userFixRounds++
		}
	}

	noun := "issue"
	if initialCount != 1 {
		noun = "issues"
	}

	parts := []string{fmt.Sprintf("%d %s found", initialCount, noun)}

	if autoFixRounds > 0 && userFixRounds > 0 {
		parts = append(parts, fmt.Sprintf("auto-fixed (%d) + user-fixed (%d)", autoFixRounds, userFixRounds))
	} else if autoFixRounds > 0 {
		parts = append(parts, "auto-fixed")
	} else if userFixRounds > 0 {
		parts = append(parts, "user-fixed")
	}

	return strings.Join(parts, " → ")
}

func buildRoundsDetail(name string, rounds []*db.StepRound) string {
	if len(rounds) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("<details>\n<summary>%s details</summary>\n\n", name))

	for _, r := range rounds {
		triggerLabel := ""
		switch r.Trigger {
		case "initial":
			triggerLabel = ""
		case "auto_fix":
			triggerLabel = " (auto-fix)"
		case "user_fix":
			triggerLabel = " (user-fix)"
		}

		if r.FindingsJSON == nil {
			b.WriteString(fmt.Sprintf("**Round %d**%s - passed ✅\n\n", r.Round, triggerLabel))
			continue
		}

		findings, err := types.ParseFindingsJSON(*r.FindingsJSON)
		if err != nil {
			b.WriteString(fmt.Sprintf("**Round %d**%s - failed to parse findings\n\n", r.Round, triggerLabel))
			continue
		}

		count := countFindingsBySeverity(&findings)
		b.WriteString(fmt.Sprintf("**Round %d**%s - found %s\n", r.Round, triggerLabel, count))

		for _, f := range findings.Items {
			emoji := severityEmoji(f.Severity)
			loc := ""
			if f.File != "" {
				loc = fmt.Sprintf("`%s", f.File)
				if f.Line > 0 {
					loc += fmt.Sprintf(":%d", f.Line)
				}
				loc += "` - "
			}
			b.WriteString(fmt.Sprintf("- %s %s%s\n", emoji, loc, f.Description))
		}
		b.WriteString("\n")
	}

	b.WriteString("</details>\n")
	return b.String()
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
	case types.StepLint:
		return "Lint"
	default:
		return string(name)
	}
}

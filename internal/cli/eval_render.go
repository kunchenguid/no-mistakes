package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/kunchenguid/no-mistakes/internal/eval"
)

// The eval dashboards reuse the stats dashboard idioms (titled box, metric
// lines, progress bars) but are wider: composition strata and warnings carry
// more text than the stats counters do.
const (
	evalBoxWidth = 79
	evalBarWidth = 20
	// evalCompositionWidth is the room one stratum line has for its repository
	// and strata columns: the box content minus the "  NNNN  " count prefix.
	evalCompositionWidth    = evalBoxWidth - 4 - 8
	minCompositionRepoWidth = 8
	compositionSeparator    = " · "
)

// renderEvalSetsDashboard renders `eval sets` with the diversified holdout as
// the headline: its size, gold composition, and the instant self-score of the
// recorded reviews against their own gold. The other sets are a compact
// footnote. Everything shown comes from InspectSets, which reads only local
// registry rows and captured files - no replay, agent, or network.
func renderEvalSetsDashboard(summaries []eval.SetSummary) string {
	byName := map[string]eval.SetSummary{}
	for _, summary := range summaries {
		byName[summary.Name] = summary
	}
	diversified := byName["diversified"]

	var lines []string
	lines = append(lines, "")
	lines = append(lines, "  Diversified holdout (official gold-only set)")
	capDetail := fmt.Sprintf("pins %d · cap %d", diversified.PinCount, diversified.Cap)
	if diversified.Cap == 0 {
		capDetail = fmt.Sprintf("pins %d · cap none (one gold case per stratum)", diversified.PinCount)
	}
	lines = append(lines, metricStatsLine("Cases", strconv.Itoa(diversified.Cases), capDetail))
	goldFindings := diversified.TruePositive + diversified.FalseNegative + diversified.FalsePositive
	lines = append(lines, metricStatsLine("Gold findings", strconv.Itoa(goldFindings), fmt.Sprintf("across %d gold case(s)", diversified.GoldCases)))
	if goldFindings > 0 {
		lines = append(lines, "")
		lines = append(lines, evalConfusionMatrixLines(diversified.TruePositive, diversified.FalseNegative, diversified.FalsePositive)...)
	}
	lines = append(lines, "")
	lines = append(lines, "  Self-score: the recorded reviews scored against their own gold")
	if diversified.SelfScore.Labeled == 0 {
		lines = append(lines, "    unlabeled / pending (no finding-level gold yet)")
	} else {
		lines = append(lines, evalScoreLines(diversified.SelfScore)...)
	}
	if len(diversified.Composition) > 0 {
		lines = append(lines, "")
		lines = append(lines, "  Composition")
		lines = append(lines, compositionLines(diversified.Composition)...)
	}

	lines = append(lines, "")
	lines = append(lines, "  Other sets")
	for _, name := range []string{"all", "labeled", "tune"} {
		summary, ok := byName[name]
		if !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-8s %4d case(s) · %d gold · %d unlabeled / pending · %d queued",
			summary.Name, summary.Cases, summary.GoldCases, summary.Unlabeled, summary.QueuedFindings))
	}

	for _, summary := range summaries {
		if summary.Warning == "" {
			continue
		}
		lines = append(lines, "")
		lines = append(lines, sYellow.Render("  ⚠ "+summary.Warning))
	}
	lines = append(lines, "")
	lines = append(lines, sDim.Render("  local-only: cases, gold, and scores never leave this machine"))
	lines = append(lines, "")
	return renderTitledBox(" eval case sets ", evalBoxWidth, lines)
}

// compositionLines renders the stratum table. The repository column is sized
// from whatever room the fixed strata leave and is resolved once for the whole
// table, so every row shows the same kind of identity: full "owner/name" when
// they all fit, otherwise the short repository name (the convention the stats
// dashboard already uses) rather than a name cut mid-word.
func compositionLines(rows []eval.CompositionRow) []string {
	widest := 0
	for _, row := range rows {
		if width := lipgloss.Width(compositionStrata(row)); width > widest {
			widest = width
		}
	}
	repoWidth := evalCompositionWidth - widest - lipgloss.Width(compositionSeparator)
	if repoWidth < minCompositionRepoWidth {
		repoWidth = minCompositionRepoWidth
	}
	names := fitRepoNames(rows, repoWidth)
	column := 0
	for _, name := range names {
		if width := lipgloss.Width(name); width > column {
			column = width
		}
	}
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		padded := names[i] + strings.Repeat(" ", column-lipgloss.Width(names[i]))
		lines = append(lines, fmt.Sprintf("  %4d  %s", row.Cases, padded+compositionSeparator+compositionStrata(row)))
	}
	return lines
}

func compositionStrata(row eval.CompositionRow) string {
	return strings.Join([]string{row.Language, row.Size, row.Severity, row.FindingType}, compositionSeparator)
}

// fitRepoNames renders every repository identity the same way: "owner/name"
// while all of them fit the column, else the bare repository name, and only a
// still-oversized name is cut.
func fitRepoNames(rows []eval.CompositionRow, width int) []string {
	names := make([]string, 0, len(rows))
	shorten := false
	for _, row := range rows {
		if lipgloss.Width(row.Repo) > width {
			shorten = true
		}
	}
	for _, row := range rows {
		name := row.Repo
		if shorten {
			if slash := strings.LastIndex(name, "/"); slash >= 0 && slash+1 < len(name) {
				name = name[slash+1:]
			}
		}
		names = append(names, truncateStatsLine(name, width))
	}
	return names
}

// evalConfusionMatrixLines renders the finding-level gold as the confusion
// matrix it actually is: what the recorded review did on the rows, what the
// human decision proved on the columns. True negatives have no cell value
// because silent agreement is never written as gold - a review that correctly
// raises nothing leaves nothing to record - so the cell reads "-" rather than
// inventing a count. A matrix of zeros still renders with its headers.
func evalConfusionMatrixLines(truePositive, falseNegative, falsePositive int) []string {
	const labelWidth = 18
	const cellWidth = 14
	const notAnIssue = "not an issue"
	cell := func(kind string, value string) string {
		return fmt.Sprintf("%-2s %6s", kind, value)
	}
	rule := strings.Repeat("─", labelWidth+cellWidth+len(notAnIssue))
	return []string{
		"  Confusion matrix (finding-level gold)",
		fmt.Sprintf("    %-*s%-*s%-*s", labelWidth, "", cellWidth, "real issue", cellWidth, notAnIssue),
		"    " + sDim.Render(rule),
		fmt.Sprintf("    %-*s%-*s%-*s", labelWidth, "review raised", cellWidth, cell("TP", strconv.Itoa(truePositive)), cellWidth, cell("FP", strconv.Itoa(falsePositive))),
		fmt.Sprintf("    %-*s%-*s%-*s", labelWidth, "review missed", cellWidth, cell("FN", strconv.Itoa(falseNegative)), cellWidth, cell("TN", "-")),
		sDim.Render("    TN is never counted: a correctly silent review leaves no gold"),
	}
}

// evalScoreLines renders one finding-level score summary with the eval
// report's semantics: recall over true-issue gold, precision as bounds with
// pending treated as FP for the lower bound, and F1 as a headline only when
// false-positive gold makes precision real.
func evalScoreLines(s eval.EvaluationSummary) []string {
	var lines []string
	trueIssues := s.TruePositive + s.FalseNegative
	if trueIssues == 0 {
		lines = append(lines, metricStatsLine("Recall", "-", "unavailable (no true-issue gold)"))
	} else {
		detail := progressBar(s.Recall(), evalBarWidth) + fmt.Sprintf("  %d/%d true issues", s.TruePositive, trueIssues)
		lines = append(lines, metricStatsLine("Recall", percent(s.Recall()), detail))
	}
	bounds := fmt.Sprintf("%s-%s", percent(s.PrecisionLower()), percent(s.Precision()))
	lines = append(lines, metricStatsLine("Precision", bounds, "pending counted as FP in the lower bound"))
	if s.HasFalsePositiveGold() {
		lines = append(lines, metricStatsLine("F1", percent(s.F1()), "headline (false-positive gold present)"))
	} else {
		lines = append(lines, metricStatsLine("F1", "-", "withheld (no false-positive gold)"))
	}
	if s.Pending > 0 {
		lines = append(lines, metricStatsLine("Pending", strconv.Itoa(s.Pending), "queued unmatched candidate finding(s)"))
	}
	return lines
}

// evalRunProgress streams one line per persisted replay so a long candidate
// comparison shows its work as it happens.
func evalRunProgress(w io.Writer, evaluation eval.Evaluation, completed, total int) {
	progress := fmt.Sprintf("%*d/%d", len(strconv.Itoa(total)), completed, total)
	if evaluation.Status != "completed" {
		fmt.Fprintf(w, "  %s %s  %s repeat %d  failed: %s\n",
			sRed.Render("✗"), progress, evaluation.CaseID, evaluation.Repeat, evaluation.Error)
		return
	}
	fmt.Fprintf(w, "  %s %s  %s repeat %d  TP %d · FN %d · FP %d · pending %d  %s\n",
		sGreen.Render("✓"), progress, evaluation.CaseID, evaluation.Repeat,
		evaluation.TruePositive, evaluation.FalseNegative, evaluation.FalsePositive, evaluation.Pending,
		formatMS(evaluation.DurationMS))
}

// renderEvalRunSummary renders the finished (or partially finished) replay
// session in the same dashboard frame as stats and eval sets.
func renderEvalRunSummary(session eval.Session, evaluations []eval.Evaluation, caseCount int) string {
	s := eval.SummarizeEvaluations(evaluations)
	var lines []string
	lines = append(lines, "")
	lines = append(lines, metricStatsLine("Candidate", "", session.Candidate))
	lines = append(lines, metricStatsLine("Case set", "", fmt.Sprintf("%s · cohort %s", session.Set, session.Cohort)))
	lines = append(lines, metricStatsLine("Replays", strconv.Itoa(s.Total), fmt.Sprintf("%d case(s) x %d repeat(s) · %d failure(s)", caseCount, session.Repeats, s.Failures)))
	lines = append(lines, metricStatsLine("Labeled", strconv.Itoa(s.Labeled), "replay(s) of cases with finding-level gold"))
	lines = append(lines, "")
	if s.Labeled == 0 {
		lines = append(lines, "  unlabeled / pending (no finding-level gold in this set yet)")
	} else {
		lines = append(lines, evalScoreLines(s)...)
	}
	lines = append(lines, "")
	if s.Total > 0 && s.TokensReported == s.Total {
		avgTokens := float64(s.FreshInputTokens+s.OutputTokens) / float64(s.Total)
		lines = append(lines, metricStatsLine("Tokens", fmt.Sprintf("%.0f", avgTokens), "fresh-input + output per replay"))
	} else {
		lines = append(lines, metricStatsLine("Tokens", "-", "unknown (not reported for every replay)"))
	}
	if s.Total > 0 {
		lines = append(lines, metricStatsLine("Wall time", formatMS(s.DurationMS/int64(s.Total)), "average per replay"))
	}
	lines = append(lines, "")
	return renderTitledBox(" eval run ", evalBoxWidth, lines)
}

package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/eval"
)

// The eval dashboards reuse the stats dashboard idioms (titled box, metric
// lines, progress bars) but are wider: composition strata and warnings carry
// more text than the stats counters do.
const (
	evalBoxWidth = 79
	evalBarWidth = 20
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
		lines = append(lines, fmt.Sprintf("    true-positive %d · false-negative %d · false-positive %d",
			diversified.TruePositive, diversified.FalseNegative, diversified.FalsePositive))
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
		for _, row := range diversified.Composition {
			lines = append(lines, fmt.Sprintf("  %4d  %s", row.Cases, compositionLabel(row)))
		}
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

func compositionLabel(row eval.CompositionRow) string {
	repo := row.Repo
	if len(repo) > 8 {
		repo = repo[:8]
	}
	return strings.Join([]string{"repo " + repo, row.Language, row.Size, row.Severity, row.FindingType}, " · ")
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

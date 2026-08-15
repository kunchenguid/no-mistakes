package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Interval is a finite-sample recall range over cases. Repeats are averaged
// inside each case so a noisy provider does not inflate apparent sample size.
type Interval struct {
	Lower float64
	Upper float64
	Cases int
}

// CandidateReport is one locally observed candidate slice.
type CandidateReport struct {
	Cohort        string
	Summary       EvaluationSummary
	RepeatCount   int
	Confidence    *Interval
	AverageTokens *float64
	AverageWallMS float64
	OnFrontier    bool
}

// Report loads every local evaluation result grouped by candidate. It never
// contacts a forge, agent provider, telemetry endpoint, or remote case store.
func Report(store *Store) ([]CandidateReport, error) {
	evaluations, err := store.evaluations()
	if err != nil {
		return nil, err
	}
	byCandidateCohort := make(map[string][]Evaluation)
	for _, evaluation := range evaluations {
		cohort := evaluation.Cohort
		if cohort == "" {
			cohort = "legacy-unmatched"
		}
		key := cohort + "\x00" + evaluation.Candidate
		byCandidateCohort[key] = append(byCandidateCohort[key], evaluation)
	}
	reports := make([]CandidateReport, 0, len(byCandidateCohort))
	for key, rows := range byCandidateCohort {
		cohort, candidate, _ := strings.Cut(key, "\x00")
		summary := SummarizeEvaluations(rows)
		repeats := repeatCount(rows)
		report := CandidateReport{
			Cohort:        cohort,
			Summary:       summary,
			RepeatCount:   repeats,
			Confidence:    confidenceInterval(candidate, rows),
			AverageWallMS: averageWallMS(rows),
		}
		if cost, ok := averageTokens(rows); ok {
			report.AverageTokens = &cost
		}
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].Cohort != reports[j].Cohort {
			return reports[i].Cohort < reports[j].Cohort
		}
		return reports[i].Summary.Candidate < reports[j].Summary.Candidate
	})
	markFrontier(reports)
	return reports, nil
}

func (s *Store) evaluations() ([]Evaluation, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("eval registry is closed")
	}
	rows, err := s.db.Query(`SELECT path FROM evaluations ORDER BY completed_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list eval results: %w", err)
	}
	defer rows.Close()
	var result []Evaluation
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan eval result: %w", err)
		}
		var evaluation Evaluation
		if err := readJSON(path, &evaluation); err != nil {
			return nil, fmt.Errorf("read eval result: %w", err)
		}
		result = append(result, evaluation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list eval results: %w", err)
	}
	return result, nil
}

func repeatCount(rows []Evaluation) int {
	seen := map[int]bool{}
	for _, row := range rows {
		seen[row.Repeat] = true
	}
	return len(seen)
}

func averageWallMS(rows []Evaluation) float64 {
	if len(rows) == 0 {
		return 0
	}
	var total int64
	for _, row := range rows {
		total += row.DurationMS
	}
	return float64(total) / float64(len(rows))
}

func averageTokens(rows []Evaluation) (float64, bool) {
	if len(rows) == 0 {
		return 0, false
	}
	var total int64
	for _, row := range rows {
		if !row.TokensReported {
			return 0, false
		}
		total += row.FreshInputTokens + row.OutputTokens
	}
	return float64(total) / float64(len(rows)), true
}

func confidenceInterval(_ string, rows []Evaluation) *Interval {
	// Each case becomes a mean recall over labeled repeats. Unlabeled
	// replays stay out of this interval and are reported as pending.
	perCase := map[string][]float64{}
	for _, row := range rows {
		if row.GoldCount == 0 {
			continue
		}
		if row.Status != "completed" {
			perCase[row.CaseID] = append(perCase[row.CaseID], 0)
			continue
		}
		denom := row.TruePositive + row.FalseNegative
		if denom == 0 {
			continue
		}
		perCase[row.CaseID] = append(perCase[row.CaseID], float64(row.TruePositive)/float64(denom))
	}
	values := make([]float64, 0, len(perCase))
	for _, scores := range perCase {
		var total float64
		for _, score := range scores {
			total += score
		}
		values = append(values, total/float64(len(scores)))
	}
	if len(values) < 2 {
		return nil
	}
	var total float64
	for _, value := range values {
		total += value
	}
	n := float64(len(values))
	proportion := total / n
	const z = 1.959963984540054
	z2 := z * z
	denominator := 1 + z2/n
	center := (proportion + z2/(2*n)) / denominator
	halfWidth := z * math.Sqrt((proportion*(1-proportion)+z2/(4*n))/n) / denominator
	return &Interval{Lower: center - halfWidth, Upper: center + halfWidth, Cases: len(values)}
}

func markFrontier(reports []CandidateReport) {
	for i := range reports {
		if reports[i].AverageTokens == nil || reports[i].Summary.Labeled == 0 || reports[i].Summary.Failures > 0 {
			continue
		}
		dominated := false
		for j := range reports {
			if i == j || reports[i].Cohort != reports[j].Cohort || reports[j].AverageTokens == nil || reports[j].Summary.Labeled == 0 || reports[j].Summary.Failures > 0 {
				continue
			}
			betterRecall := reports[j].Summary.Recall() >= reports[i].Summary.Recall()
			cheaper := *reports[j].AverageTokens <= *reports[i].AverageTokens
			strict := reports[j].Summary.Recall() > reports[i].Summary.Recall() || *reports[j].AverageTokens < *reports[i].AverageTokens
			if betterRecall && cheaper && strict {
				dominated = true
				break
			}
		}
		reports[i].OnFrontier = !dominated
	}
}

// SetSummary lets users inspect corpus coverage before an eval consumes tokens.
type SetSummary struct {
	Name           string
	Cases          int
	GoldCases      int
	TruePositive   int
	FalseNegative  int
	FalsePositive  int
	Unlabeled      int
	QueuedFindings int
	PinCount       int
	Cap            int
	Warning        string
	Composition    map[string]int
}

// InspectSets summarizes all logical sets and their diversified mix.
func InspectSets(store *Store) ([]SetSummary, error) {
	sets := []string{"all", "labeled", "diversified", "tune"}
	all, err := store.ListCases("all")
	if err != nil {
		return nil, err
	}
	labeledCount := 0
	for _, c := range all {
		if c.Labels.HasGold() {
			labeledCount++
		}
	}
	result := make([]SetSummary, 0, len(sets))
	for _, name := range sets {
		cases, err := store.ListCases(name)
		if err != nil {
			return nil, err
		}
		summary := SetSummary{Name: name, Cases: len(cases), Composition: map[string]int{}, Cap: store.diversifiedSize}
		if name == "diversified" {
			if n, err := store.pinCount(); err == nil {
				summary.PinCount = n
			}
			if len(cases) == 0 && labeledCount == 0 {
				summary.Warning = "diversified is empty: no labeled gold (unlabeled cases are not filled)"
			}
		}
		if name == "tune" && len(cases) == 0 && labeledCount > 0 {
			summary.Warning = "tune is empty; do not fit matcher thresholds on diversified"
		}
		for _, c := range cases {
			if c.Labels.HasGold() {
				summary.GoldCases++
				for _, finding := range c.Labels.Findings {
					switch finding.Kind {
					case GoldTruePositive:
						summary.TruePositive++
					case GoldFalseNegative:
						summary.FalseNegative++
					case GoldFalsePositive:
						summary.FalsePositive++
					}
				}
			} else {
				summary.Unlabeled++
			}
			summary.QueuedFindings += c.Labels.QueuedCandidateFindings
			language, size, severity := caseComposition(c)
			ftype := findingType(c)
			summary.Composition["repo="+shortFingerprint(c.RepoFingerprint)+", language="+language+", size="+size+", severity="+severity+", type="+ftype]++
		}
		result = append(result, summary)
	}
	return result, nil
}

func shortFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

// RenderSets is a stable human-readable preflight. It intentionally contains
// counts and buckets only, never source paths, URLs, diffs, or findings.
func RenderSets(summaries []SetSummary) string {
	var b strings.Builder
	b.WriteString("LOCAL-ONLY EVAL CASE SETS\n")
	for _, summary := range summaries {
		fmt.Fprintf(&b, "\n%s: %d cases, %d with finding-level gold (true-positive %d, false-negative %d, false-positive %d), %d unlabeled / pending, %d candidate findings queued\n",
			summary.Name, summary.Cases, summary.GoldCases, summary.TruePositive, summary.FalseNegative, summary.FalsePositive, summary.Unlabeled, summary.QueuedFindings)
		if summary.Name == "diversified" {
			if summary.Cap == 0 {
				fmt.Fprintf(&b, "  cap: none (one gold case per stratum), pins: %d\n", summary.PinCount)
			} else {
				fmt.Fprintf(&b, "  cap: %d, pins: %d\n", summary.Cap, summary.PinCount)
			}
		}
		if summary.Warning != "" {
			fmt.Fprintf(&b, "  warning: %s\n", summary.Warning)
		}
		keys := make([]string, 0, len(summary.Composition))
		for key := range summary.Composition {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "  %d  %s\n", summary.Composition[key], key)
		}
	}
	return b.String()
}

// RenderReport is a stable human-readable local comparison. Scores are
// finding-level. Unmatched candidate findings stay pending and are never
// called false positives. A replay with no gold is unlabeled, not a pass.
func RenderReport(reports []CandidateReport) string {
	if len(reports) == 0 {
		return "LOCAL-ONLY EVAL REPORT\nno candidate replays recorded yet\n"
	}
	var b strings.Builder
	b.WriteString("LOCAL-ONLY EVAL REPORT\n")
	for _, report := range reports {
		s := report.Summary
		fmt.Fprintf(&b, "\n%s (cohort %s)\n", s.Candidate, report.Cohort)
		fmt.Fprintf(&b, "  replays: %d across %d repeat(s); labeled: %d; failures: %d\n", s.Total, report.RepeatCount, s.Labeled, s.Failures)
		if s.Labeled == 0 {
			b.WriteString("  finding scores: unlabeled / pending (no finding-level gold yet)\n")
		} else {
			fmt.Fprintf(&b, "  finding scores: true-positive %d, false-negative %d, false-positive %d, pending %d\n", s.TruePositive, s.FalseNegative, s.FalsePositive, s.Pending)
			if s.TruePositive+s.FalseNegative == 0 {
				b.WriteString("  recall: unavailable (no true-issue gold)\n")
			} else {
				fmt.Fprintf(&b, "  recall: %.1f%% (%d/%d gold issues)\n", 100*s.Recall(), s.TruePositive, s.TruePositive+s.FalseNegative)
				if s.TruePositiveFuzzy > 0 {
					fmt.Fprintf(&b, "  recall-if-exact-only: %.1f%% (%d/%d)\n", 100*s.ExactRecall(), s.TruePositiveExact, s.TruePositive+s.FalseNegative)
				}
				if report.Confidence != nil {
					fmt.Fprintf(&b, "  case-level recall range: %.1f%%-%.1f%% over %d case(s)\n", 100*report.Confidence.Lower, 100*report.Confidence.Upper, report.Confidence.Cases)
				}
			}
			fmt.Fprintf(&b, "  precision bounds: %.1f%%-%.1f%% (adjudicated %.1f%%; pending treated as FP for the lower bound)\n",
				100*s.PrecisionLower(), 100*s.Precision(), 100*s.Precision())
			if s.HasFalsePositiveGold() {
				fmt.Fprintf(&b, "  F1: %.1f%% (headline; false-positive gold is present)\n", 100*s.F1())
			} else {
				fmt.Fprintf(&b, "  F1: withheld (no false-positive gold; precision in [%.1f%%, %.1f%%])\n", 100*s.PrecisionLower(), 100*s.Precision())
			}
		}
		if s.Pending > 0 {
			fmt.Fprintf(&b, "  queued unmatched candidate findings: %d (not scored as false-positive)\n", s.Pending)
		}
		if report.AverageTokens == nil {
			b.WriteString("  token cost: unknown (token usage was not reported for every replay)\n")
		} else {
			fmt.Fprintf(&b, "  token cost: %.0f fresh-input + output tokens per reported replay\n", *report.AverageTokens)
		}
		fmt.Fprintf(&b, "  wall time: %.1fs average\n", report.AverageWallMS/1000)
		if report.AverageTokens != nil {
			if s.TruePositive+s.FalseNegative == 0 {
				b.WriteString("  recall-vs-cost frontier: unavailable (no true-issue gold)\n")
			} else {
				fmt.Fprintf(&b, "  recall-vs-cost frontier: %t\n", report.OnFrontier)
			}
		}
	}
	return b.String()
}

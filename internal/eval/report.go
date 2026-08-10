package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Interval is a two-sided 95% Wilson score interval over cases. Cases are the
// independent unit; repeats are averaged inside each case so a noisy provider
// does not inflate apparent sample size.
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
	// First turn each case into a mean over CONCLUSIVE repeats. A pending
	// unexpected park stays out of this conditional interval and is surfaced
	// separately in every report as a queue count and a lower-bound accuracy.
	perCase := map[string][]float64{}
	for _, row := range rows {
		if row.ExpectedPark == nil {
			continue
		}
		if row.Status != "completed" {
			perCase[row.CaseID] = append(perCase[row.CaseID], 0)
			continue
		}
		var score *float64
		switch {
		case *row.ExpectedPark && row.CandidateParked:
			v := 1.0
			score = &v
		case *row.ExpectedPark && !row.CandidateParked:
			v := 0.0
			score = &v
		case !*row.ExpectedPark && !row.CandidateParked:
			v := 1.0
			score = &v
		}
		if score != nil {
			perCase[row.CaseID] = append(perCase[row.CaseID], *score)
		}
	}
	values := make([]float64, 0, len(perCase))
	for _, scores := range perCase {
		var total float64
		for _, score := range scores {
			total += score
		}
		values = append(values, total/float64(len(scores)))
	}
	if len(values) == 0 {
		return nil
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
			betterAccuracy := reports[j].Summary.LowerBoundAccuracy() >= reports[i].Summary.LowerBoundAccuracy()
			cheaper := *reports[j].AverageTokens <= *reports[i].AverageTokens
			strict := reports[j].Summary.LowerBoundAccuracy() > reports[i].Summary.LowerBoundAccuracy() || *reports[j].AverageTokens < *reports[i].AverageTokens
			if betterAccuracy && cheaper && strict {
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
	VerdictLabels  int
	ShouldPark     int
	ShouldPass     int
	QueuedFindings int
	Composition    map[string]int
}

// InspectSets summarizes all logical MVP sets and their diversified mix.
func InspectSets(store *Store) ([]SetSummary, error) {
	sets := []string{"all", "labeled", "diversified"}
	result := make([]SetSummary, 0, len(sets))
	for _, name := range sets {
		cases, err := store.ListCases(name)
		if err != nil {
			return nil, err
		}
		summary := SetSummary{Name: name, Cases: len(cases), Composition: map[string]int{}}
		for _, c := range cases {
			if c.Labels.Verdict.Known {
				summary.VerdictLabels++
				if c.Labels.Verdict.ShouldPark {
					summary.ShouldPark++
				} else {
					summary.ShouldPass++
				}
			}
			summary.QueuedFindings += c.Labels.QueuedCandidateFindings
			language, size, severity := caseComposition(c)
			summary.Composition["repo="+shortFingerprint(c.RepoFingerprint)+", language="+language+", size="+size+", severity="+severity]++
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
		fmt.Fprintf(&b, "\n%s: %d cases, %d verdict labels (park %d, pass %d), %d candidate findings queued\n", summary.Name, summary.Cases, summary.VerdictLabels, summary.ShouldPark, summary.ShouldPass, summary.QueuedFindings)
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

// RenderReport is a stable human-readable local comparison. Confidence
// intervals are conditional on conclusive verdicts; the lower-bound metric
// includes queued unexpected parks so the uncertainty is explicit.
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
			b.WriteString("  verdict agreement: no human-confirmed verdict labels yet\n")
		} else {
			fmt.Fprintf(&b, "  confirmed verdict agreement: %.1f%% (%d/%d); conservative lower bound: %.1f%%\n", 100*s.ConfirmedAccuracy(), s.Correct, s.Conclusive, 100*s.LowerBoundAccuracy())
			if report.Confidence != nil {
				fmt.Fprintf(&b, "  95%% Wilson score CI: %.1f%%-%.1f%% over %d case(s)\n", 100*report.Confidence.Lower, 100*report.Confidence.Upper, report.Confidence.Cases)
			}
			if s.UnexpectedParks > 0 {
				fmt.Fprintf(&b, "  queued unexpected parks: %d (not scored wrong pending finding-level adjudication)\n", s.UnexpectedParks)
			}
		}
		if report.AverageTokens == nil {
			b.WriteString("  token cost: unknown (token usage was not reported for every replay)\n")
		} else {
			fmt.Fprintf(&b, "  token cost: %.0f fresh-input + output tokens per reported replay\n", *report.AverageTokens)
		}
		fmt.Fprintf(&b, "  wall time: %.1fs average\n", report.AverageWallMS/1000)
		if report.AverageTokens != nil {
			fmt.Fprintf(&b, "  accuracy-vs-cost frontier: %t\n", report.OnFrontier)
		}
	}
	return b.String()
}

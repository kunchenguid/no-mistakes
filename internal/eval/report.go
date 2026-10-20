package eval

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
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

// CandidateReport is one locally observed candidate slice: one candidate, one
// cohort, and one review prompt variant (see Report).
type CandidateReport struct {
	Cohort string
	// Build names the binary or binaries that ran this slice's sessions, or
	// "not recorded" for sessions that predate the field.
	Build string
	// PromptFingerprint digests the per-case review prompt and config
	// fingerprints this slice's replays ran under. It is empty when none was
	// recorded.
	PromptFingerprint string
	Completeness      Completeness
	// Incomplete marks a slice whose replays do not cover its recorded plans:
	// a replay is missing or failed, or a plan could not be read. Such a slice
	// never joins the recall-vs-cost frontier.
	Incomplete  bool
	Summary     EvaluationSummary
	RepeatCount int
	Confidence  *Interval
	// AverageTokens is the honest per-replay total: fresh-input + output plus
	// cache-read and cache-write where the adapter reported them. Cache reads
	// dominate review cost by orders of magnitude, so ranking the
	// recall-vs-cost frontier on fresh input alone would crown whichever
	// provider cached hardest as cheapest.
	AverageTokens           *float64
	AverageCacheReadTokens  *float64
	AverageCacheWriteTokens *float64
	AverageWallMS           float64
	OnFrontier              bool
}

// Completeness compares the replays a slice's sessions planned with the ones
// that were recorded and completed. A session that stopped early leaves
// planned replays unrecorded; without this the report would score only the
// cases that ran and look finished.
type Completeness struct {
	// PlansKnown is false when any session's plan file is missing or
	// unreadable, so the planned counts below are a lower bound.
	PlansKnown       bool
	Sessions         int
	PlannedCases     int
	CompletedCases   int
	PlannedReplays   int
	RecordedReplays  int
	CompletedReplays int
}

// Missing is the number of planned replays that were never recorded.
func (c Completeness) Missing() int {
	if c.RecordedReplays >= c.PlannedReplays {
		return 0
	}
	return c.PlannedReplays - c.RecordedReplays
}

// Failed is the number of recorded replays that did not complete.
func (c Completeness) Failed() int { return c.RecordedReplays - c.CompletedReplays }

// Complete reports whether every planned replay was recorded and completed.
func (c Completeness) Complete() bool {
	return c.PlansKnown && c.Missing() == 0 && c.Failed() == 0
}

// Report loads every local evaluation result grouped by cohort, candidate, and
// review prompt variant. It never contacts a forge, agent provider, telemetry
// endpoint, or remote case store.
//
// The cohort separates builds, while the prompt fingerprint also captures
// the effective configuration and delivered prompt for each case.
// Sessions of one cohort and candidate share a slice only when their recorded
// prompts identify exactly one compatible variant. Sessions with no recorded
// prompts or multiple compatible variants stay in an unknown-variant slice.
func Report(store *Store) ([]CandidateReport, error) {
	evaluations, err := store.evaluations()
	if err != nil {
		return nil, err
	}
	type bucket struct {
		cohort    string
		candidate string
		sessions  []string
		rows      map[string][]Evaluation
	}
	var order []string
	buckets := make(map[string]*bucket)
	for _, evaluation := range evaluations {
		cohort := evaluation.Cohort
		if cohort == "" {
			cohort = "legacy-unmatched"
		}
		key := cohort + "\x00" + evaluation.Candidate
		b := buckets[key]
		if b == nil {
			b = &bucket{cohort: cohort, candidate: evaluation.Candidate, rows: make(map[string][]Evaluation)}
			buckets[key] = b
			order = append(order, key)
		}
		if _, seen := b.rows[evaluation.SessionID]; !seen {
			b.sessions = append(b.sessions, evaluation.SessionID)
		}
		b.rows[evaluation.SessionID] = append(b.rows[evaluation.SessionID], evaluation)
	}
	plans := make(map[string]*Session)
	plan := func(id string) *Session {
		if p, ok := plans[id]; ok {
			return p
		}
		p := store.readSession(id)
		plans[id] = p
		return p
	}
	entries, err := os.ReadDir(filepath.Join(store.root, "sessions"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("list eval sessions: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		p := plan(id)
		if p == nil {
			continue
		}
		cohort := p.Cohort
		if cohort == "" {
			cohort = "legacy-unmatched"
		}
		key := cohort + "\x00" + p.Candidate
		b := buckets[key]
		if b == nil {
			b = &bucket{cohort: cohort, candidate: p.Candidate, rows: make(map[string][]Evaluation)}
			buckets[key] = b
			order = append(order, key)
		}
		if _, seen := b.rows[id]; !seen {
			b.sessions = append(b.sessions, id)
			b.rows[id] = nil
		}
	}
	var reports []CandidateReport
	for _, key := range order {
		b := buckets[key]
		for _, variant := range promptVariants(b.sessions, b.rows) {
			var rows []Evaluation
			sessionPlans := make([]*Session, 0, len(variant.sessions))
			for _, id := range variant.sessions {
				rows = append(rows, b.rows[id]...)
				sessionPlans = append(sessionPlans, plan(id))
			}
			completeness := sliceCompleteness(sessionPlans, rows)
			report := CandidateReport{
				Cohort:            b.cohort,
				Build:             sliceBuild(sessionPlans),
				PromptFingerprint: variant.fingerprint(),
				Completeness:      completeness,
				Incomplete:        !completeness.Complete(),
				Summary:           SummarizeEvaluations(rows),
				RepeatCount:       repeatCount(rows),
				Confidence:        confidenceInterval("", rows),
				AverageWallMS:     averageWallMS(rows),
			}
			report.Summary.Candidate = b.candidate
			if total, cacheRead, cacheWrite, ok := averageTokens(rows); ok {
				report.AverageTokens = &total
				report.AverageCacheReadTokens = &cacheRead
				report.AverageCacheWriteTokens = &cacheWrite
			}
			reports = append(reports, report)
		}
	}
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].Cohort != reports[j].Cohort {
			return reports[i].Cohort < reports[j].Cohort
		}
		if reports[i].Summary.Candidate != reports[j].Summary.Candidate {
			return reports[i].Summary.Candidate < reports[j].Summary.Candidate
		}
		return reports[i].PromptFingerprint < reports[j].PromptFingerprint
	})
	markFrontier(reports)
	return reports, nil
}

// promptVariant is a set of sessions that agree on every shared case's prompt
// fingerprints. prompts maps a case to its sorted, comma-joined fingerprints;
// a case with none recorded is absent and conflicts with nothing.
type promptVariant struct {
	sessions []string
	prompts  map[string]string
}

func (v promptVariant) fingerprint() string {
	if len(v.prompts) == 0 {
		return ""
	}
	caseIDs := make([]string, 0, len(v.prompts))
	for caseID := range v.prompts {
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)
	h := sha256.New()
	for _, caseID := range caseIDs {
		fmt.Fprintf(h, "%s\x00%s\x00", caseID, v.prompts[caseID])
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// promptVariants discovers variants before assigning sessions, so a partial
// session cannot hide its ambiguity by joining before a second variant appears.
func promptVariants(sessions []string, rows map[string][]Evaluation) []promptVariant {
	var variants []promptVariant
	for _, id := range sessions {
		prompts := sessionPrompts(rows[id])
		if len(prompts) == 0 {
			continue
		}
		placed := false
		for i := range variants {
			if !promptsAgree(variants[i].prompts, prompts) {
				continue
			}
			for caseID, fingerprints := range prompts {
				variants[i].prompts[caseID] = fingerprints
			}
			placed = true
			break
		}
		if !placed {
			variants = append(variants, promptVariant{prompts: prompts})
		}
	}
	var unknown promptVariant
	for _, id := range sessions {
		prompts := sessionPrompts(rows[id])
		match, matches := -1, 0
		if len(prompts) > 0 {
			for i := range variants {
				if promptsAgree(variants[i].prompts, prompts) {
					match = i
					matches++
				}
			}
		}
		if matches == 1 {
			variants[match].sessions = append(variants[match].sessions, id)
		} else {
			unknown.sessions = append(unknown.sessions, id)
		}
	}
	// A variant merged from sessions that all turned out ambiguous describes
	// a prompt combination nobody ran, so it must not become a report row.
	assigned := variants[:0]
	for _, variant := range variants {
		if len(variant.sessions) > 0 {
			assigned = append(assigned, variant)
		}
	}
	if len(unknown.sessions) > 0 {
		assigned = append(assigned, unknown)
	}
	return assigned
}

func sessionPrompts(rows []Evaluation) map[string]string {
	byCase := make(map[string]map[string]bool)
	for _, row := range rows {
		if row.PromptFingerprint == "" {
			continue
		}
		if byCase[row.CaseID] == nil {
			byCase[row.CaseID] = make(map[string]bool)
		}
		byCase[row.CaseID][row.PromptFingerprint] = true
	}
	prompts := make(map[string]string, len(byCase))
	for caseID, set := range byCase {
		fingerprints := make([]string, 0, len(set))
		for fingerprint := range set {
			fingerprints = append(fingerprints, fingerprint)
		}
		sort.Strings(fingerprints)
		prompts[caseID] = strings.Join(fingerprints, ",")
	}
	return prompts
}

func promptsAgree(a, b map[string]string) bool {
	for caseID, fingerprints := range b {
		if existing, ok := a[caseID]; ok && existing != fingerprints {
			return false
		}
	}
	return true
}

// sliceCompleteness counts a case as completed only when every replay any of
// the slice's sessions planned for it completed.
func sliceCompleteness(plans []*Session, rows []Evaluation) Completeness {
	c := Completeness{PlansKnown: true, Sessions: len(plans)}
	planned := make(map[string]int)
	for _, plan := range plans {
		if plan == nil {
			c.PlansKnown = false
			continue
		}
		for _, caseID := range plan.CaseIDs {
			planned[caseID] += plan.Repeats
			c.PlannedReplays += plan.Repeats
		}
	}
	completed := make(map[string]int)
	for _, row := range rows {
		c.RecordedReplays++
		if row.Status == "completed" {
			c.CompletedReplays++
			completed[row.CaseID]++
		}
	}
	c.PlannedCases = len(planned)
	for caseID, want := range planned {
		if completed[caseID] >= want {
			c.CompletedCases++
		}
	}
	return c
}

func sliceBuild(plans []*Session) string {
	seen := make(map[string]bool)
	var builds []string
	for _, plan := range plans {
		build := ReplayBuild{}.String()
		if plan != nil {
			build = plan.Build.String()
		}
		if !seen[build] {
			seen[build] = true
			builds = append(builds, build)
		}
	}
	sort.Strings(builds)
	return strings.Join(builds, ", ")
}

// readSession loads a replay plan written by prepareReplay, or nil when it is
// missing or unreadable; the report then calls the plan unknown instead of
// failing.
func (s *Store) readSession(id string) *Session {
	if id == "" || id != filepath.Base(id) {
		return nil
	}
	var session Session
	if err := readJSON(filepath.Join(s.root, "sessions", id+".json"), &session); err != nil {
		return nil
	}
	return &session
}

func (s *Store) evaluations() ([]Evaluation, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("eval registry is closed")
	}
	rows, err := s.db.Query(`SELECT path, input_tokens, fresh_input_tokens, cache_read_tokens, cache_write_tokens FROM evaluations ORDER BY completed_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list eval results: %w", err)
	}
	defer rows.Close()
	var result []Evaluation
	for rows.Next() {
		var path string
		var input, fresh int64
		var cacheRead, cacheWrite sql.NullInt64
		if err := rows.Scan(&path, &input, &fresh, &cacheRead, &cacheWrite); err != nil {
			return nil, fmt.Errorf("scan eval result: %w", err)
		}
		var evaluation Evaluation
		if err := readJSON(path, &evaluation); err != nil {
			return nil, fmt.Errorf("read eval result: %w", err)
		}
		evaluation.InputTokens = input
		evaluation.FreshInputTokens = fresh
		evaluation.CacheReadTokens = cacheRead.Int64
		evaluation.CacheWriteTokens = cacheWrite.Int64
		// A skipped historical backfill cannot certify the token total.
		evaluation.TokensReported = evaluation.TokensReported && cacheRead.Valid && cacheWrite.Valid
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

func averageTokens(rows []Evaluation) (total, cacheRead, cacheWrite float64, ok bool) {
	if len(rows) == 0 {
		return 0, 0, 0, false
	}
	var freshAndOutput, reads, writes int64
	for _, row := range rows {
		if !row.TokensReported {
			return 0, 0, 0, false
		}
		freshAndOutput += row.FreshInputTokens + row.OutputTokens
		reads += row.CacheReadTokens
		writes += row.CacheWriteTokens
	}
	n := float64(len(rows))
	return float64(freshAndOutput+reads+writes) / n, float64(reads) / n, float64(writes) / n, true
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
		if !frontierEligible(reports[i]) {
			continue
		}
		dominated := false
		for j := range reports {
			if i == j || reports[i].Cohort != reports[j].Cohort || !frontierEligible(reports[j]) {
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

func frontierEligible(report CandidateReport) bool {
	return report.AverageTokens != nil && report.Summary.Labeled > 0 && report.Summary.Failures == 0 && !report.Incomplete
}

// SetSummary lets users inspect corpus coverage before an eval consumes tokens.
// SelfScore is the recorded source reviews of the set scored against their own
// gold (see SelfScoreRecordedReviews); it is computed from already-captured
// local files, never from a fresh replay.
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
	Composition    []CompositionRow
	SelfScore      EvaluationSummary
}

// CompositionRow is one stratum bucket of a case set: the same axes the
// diversified holdout stratifies on.
type CompositionRow struct {
	// Repo is the repository's display identity: its resolved name when the
	// store was given one (see Store.SetRepoNames), else the short fingerprint.
	Repo        string
	Language    string
	Size        string
	Severity    string
	FindingType string
	Cases       int
}

// InspectSets summarizes all logical sets and their diversified mix. It reads
// only local registry rows and captured case files, so it stays instant no
// matter how expensive a replay of the same sets would be.
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
	queuedByCase, err := store.pendingFindingCounts()
	if err != nil {
		return nil, err
	}
	result := make([]SetSummary, 0, len(sets))
	for _, name := range sets {
		cases, err := store.ListCases(name)
		if err != nil {
			return nil, err
		}
		summary := SetSummary{Name: name, Cases: len(cases), Cap: store.diversifiedSize, SelfScore: SelfScoreRecordedReviews(cases)}
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
		type compositionKey struct {
			repoFingerprint string
			language        string
			size            string
			severity        string
			findingType     string
		}
		composition := map[compositionKey]int{}
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
			summary.QueuedFindings += queuedByCase[c.ID]
			language, size, severity := caseComposition(c)
			composition[compositionKey{
				repoFingerprint: c.RepoFingerprint,
				language:        language,
				size:            size,
				severity:        severity,
				findingType:     findingType(c),
			}]++
		}
		rows := make([]CompositionRow, 0, len(composition))
		for key, n := range composition {
			rows = append(rows, CompositionRow{
				Repo:        store.repoDisplay(key.repoFingerprint),
				Language:    key.language,
				Size:        key.size,
				Severity:    key.severity,
				FindingType: key.findingType,
				Cases:       n,
			})
		}
		summary.Composition = sortedCompositionRows(rows)
		result = append(result, summary)
	}
	return result, nil
}

func sortedCompositionRows(rows []CompositionRow) []CompositionRow {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Language != b.Language {
			return a.Language < b.Language
		}
		if a.Size != b.Size {
			return a.Size < b.Size
		}
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.FindingType != b.FindingType {
			return a.FindingType < b.FindingType
		}
		return a.Cases < b.Cases
	})
	return rows
}

// SelfScoreRecordedReviews scores each case's recorded source-review findings
// against that case's own gold, exactly as a replayed candidate would be
// scored. Everything it reads was captured when the case was frozen, so the
// result is available instantly without invoking an agent, touching a gate, or
// re-running anything. It answers "what would eval report for the reviews that
// produced this set" - the baseline a candidate has to beat.
func SelfScoreRecordedReviews(cases []Case) EvaluationSummary {
	evaluations := make([]Evaluation, 0, len(cases))
	for _, c := range cases {
		evaluation := Evaluation{
			CaseID:            c.ID,
			Candidate:         "recorded-review",
			Status:            "completed",
			HasFindingGold:    c.Labels.HasGold(),
			GoldCount:         c.Labels.TrueIssueCount(),
			FalsePositiveGold: c.Labels.FalsePositiveCount(),
		}
		findings, err := osReadRoundFindings(c)
		if err != nil {
			evaluation.Status = "failed"
		} else {
			score := ScoreCandidate(c.Labels, findings)
			evaluation.TruePositive = score.TruePositive
			evaluation.TruePositiveExact = score.TruePositiveExact
			evaluation.TruePositiveFuzzy = score.TruePositiveFuzzy
			evaluation.FalseNegative = score.FalseNegative
			evaluation.FalsePositive = score.FalsePositive
			evaluation.FalsePositiveGold = score.FalsePositiveGold
			evaluation.Pending = score.Pending
		}
		evaluations = append(evaluations, evaluation)
	}
	return SummarizeEvaluations(evaluations)
}

func shortFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func renderCompleteness(c Completeness) string {
	plan := fmt.Sprintf("%d of %d planned case(s) and %d of %d planned replay(s) completed across %d session(s)",
		c.CompletedCases, c.PlannedCases, c.CompletedReplays, c.PlannedReplays, c.Sessions)
	switch {
	case !c.PlansKnown:
		return fmt.Sprintf("  completeness: unknown - a session plan was not recorded; %d of %d recorded replay(s) completed\n", c.CompletedReplays, c.RecordedReplays)
	case c.Complete():
		return "  completeness: complete - " + plan + "\n"
	default:
		return fmt.Sprintf("  completeness: incomplete - %s (%d missing, %d failed); scores cover only the recorded replays\n", plan, c.Missing(), c.Failed())
	}
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
		marker := ""
		if report.Incomplete {
			marker = " INCOMPLETE"
		}
		fmt.Fprintf(&b, "\n%s (cohort %s)%s\n", s.Candidate, report.Cohort, marker)
		prompt := report.PromptFingerprint
		if prompt == "" {
			prompt = "not recorded"
		}
		build := report.Build
		if build == "" {
			build = ReplayBuild{}.String()
		}
		fmt.Fprintf(&b, "  build: %s; review prompt+config fingerprint: %s\n", build, prompt)
		b.WriteString(renderCompleteness(report.Completeness))
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
			cacheRead, cacheWrite := 0.0, 0.0
			if report.AverageCacheReadTokens != nil {
				cacheRead = *report.AverageCacheReadTokens
			}
			if report.AverageCacheWriteTokens != nil {
				cacheWrite = *report.AverageCacheWriteTokens
			}
			line := fmt.Sprintf("  token cost: %.0f fresh-input + output", *report.AverageTokens-cacheRead-cacheWrite)
			if cacheRead > 0 {
				line += fmt.Sprintf(" + %.0f cache-read", cacheRead)
			}
			if cacheWrite > 0 {
				line += fmt.Sprintf(" + %.0f cache-write", cacheWrite)
			}
			if cacheRead > 0 || cacheWrite > 0 {
				line += fmt.Sprintf(" = %.0f tokens per reported replay\n", *report.AverageTokens)
			} else {
				line += " tokens per reported replay\n"
			}
			b.WriteString(line)
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

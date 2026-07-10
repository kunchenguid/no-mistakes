package db

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// canaryCohortSize is the frozen size of each canary cohort: the ten runs
// immediately before routing activated, and the first ten routed runs after.
const canaryCohortSize = 10

const (
	canaryCohortBaseline = "baseline"
	canaryCohortRouted   = "routed"
)

// canaryTargetReduction is the advisory median-latency improvement the routed
// cohort aims for over the frozen baseline. It is reporting only and never
// changes Profiles, Routes, circuits, or gate outcomes.
const canaryTargetReduction = 0.30

// canaryRetainedPercent is the integer complement of the target reduction: the
// routed median must land at or below this percent of the baseline median to
// meet the advisory target. Kept as an integer so the comparison is exact.
const canaryRetainedPercent = 70

// CanaryRunFacts is one run's frozen workload measurement in a cohort. A
// changed-file or changed-line count of -1 means the fact was unavailable when
// the run was recorded (for example a baseline run whose diff was not
// recomputed).
type CanaryRunFacts struct {
	RunID           string
	CompletedAt     int64
	ExecutionMS     int64
	InvocationMS    int64
	Escalations     int
	Failovers       int
	ChangedFiles    int
	ChangedLines    int
	InitialFindings int
}

// CanaryCohort is an ordered, frozen set of run facts and its derived median of
// the execution-only agent-bearing Step-round metric.
type CanaryCohort struct {
	Runs         []CanaryRunFacts
	Complete     bool
	MedianExecMS int64
}

// CanaryReport compares the frozen baseline cohort against the routed cohort. It
// is advisory only.
type CanaryReport struct {
	Activated       bool
	ActivatedAt     int64
	Fingerprint     string
	Baseline        CanaryCohort
	Routed          CanaryCohort
	TargetReduction float64
	// Met is nil while the comparison is not yet computable (either cohort
	// empty); otherwise it reports whether the routed median met the target.
	Met *bool
}

// canaryQueryer is the read surface shared by *sql.DB and *sql.Tx so run-fact
// computation works inside or outside the activation transaction.
type canaryQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// IsCanaryActivated reports whether the canary policy has been activated. The
// canary is dormant until activation: collection and comparison do nothing
// before it.
func (d *DB) IsCanaryActivated() (bool, error) {
	var n int
	if err := d.sql.QueryRow(`SELECT count(*) FROM canary_activation WHERE id = 1`).Scan(&n); err != nil {
		return false, fmt.Errorf("check canary activation: %w", err)
	}
	return n > 0, nil
}

// ActivateCanary idempotently freezes the baseline cohort — the ten most recent
// completed runs and their workload facts — the moment the routing policy
// activates. changedStats supplies git-derived changed file/line counts where
// available; a nil function or a false ok records them as unavailable (-1). It
// returns true when this call performed activation, false if already active.
func (d *DB) ActivateCanary(fingerprint string, changedStats func(baseSHA, headSHA string) (files, lines int, ok bool)) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("begin canary activation: %w", err)
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRow(`SELECT count(*) FROM canary_activation WHERE id = 1`).Scan(&existing); err != nil {
		return false, fmt.Errorf("check canary activation: %w", err)
	}
	if existing > 0 {
		return false, nil
	}

	if _, err := tx.Exec(`INSERT INTO canary_activation (id, activated_at, fingerprint) VALUES (1, ?, ?)`, now(), fingerprint); err != nil {
		return false, fmt.Errorf("record canary activation: %w", err)
	}

	type baselineRun struct{ id, baseSHA, headSHA string }
	var baseline []baselineRun
	rows, err := tx.Query(`SELECT id, base_sha, head_sha FROM runs WHERE status = ? ORDER BY updated_at DESC, id DESC LIMIT ?`, types.RunCompleted, canaryCohortSize)
	if err != nil {
		return false, fmt.Errorf("select baseline runs: %w", err)
	}
	for rows.Next() {
		var r baselineRun
		if err := rows.Scan(&r.id, &r.baseSHA, &r.headSHA); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan baseline run: %w", err)
		}
		baseline = append(baseline, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate baseline runs: %w", err)
	}

	for pos, r := range baseline {
		facts, err := computeCanaryRunFacts(tx, r.id)
		if err != nil {
			return false, err
		}
		facts.ChangedFiles, facts.ChangedLines = -1, -1
		if changedStats != nil {
			if files, lines, ok := changedStats(r.baseSHA, r.headSHA); ok {
				facts.ChangedFiles, facts.ChangedLines = files, lines
			}
		}
		if err := insertCanaryCohortRun(tx, canaryCohortBaseline, pos, facts); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit canary activation: %w", err)
	}
	return true, nil
}

// RecordRoutedRunInCanary adds one successful routed run to the routed cohort
// when the canary is active, the run completed after activation, the cohort is
// not yet full, and the run is not already recorded. It never replaces an
// existing member, and failed, cancelled, pre-activation, and duplicate runs
// are ignored. Returns true when the run was added.
func (d *DB) RecordRoutedRunInCanary(runID string, changedFiles, changedLines int) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("begin canary intake: %w", err)
	}
	defer tx.Rollback()

	var activatedAt int64
	switch err := tx.QueryRow(`SELECT activated_at FROM canary_activation WHERE id = 1`).Scan(&activatedAt); err {
	case nil:
	case sql.ErrNoRows:
		return false, nil // dormant
	default:
		return false, fmt.Errorf("check canary activation: %w", err)
	}

	var status string
	var completedAt int64
	switch err := tx.QueryRow(`SELECT status, updated_at FROM runs WHERE id = ?`, runID).Scan(&status, &completedAt); err {
	case nil:
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, fmt.Errorf("load canary run: %w", err)
	}
	if types.RunStatus(status) != types.RunCompleted || completedAt < activatedAt {
		return false, nil
	}

	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM canary_cohort_runs WHERE cohort = ?`, canaryCohortRouted).Scan(&count); err != nil {
		return false, fmt.Errorf("count routed cohort: %w", err)
	}
	if count >= canaryCohortSize {
		return false, nil
	}
	var present int
	if err := tx.QueryRow(`SELECT count(*) FROM canary_cohort_runs WHERE cohort = ? AND run_id = ?`, canaryCohortRouted, runID).Scan(&present); err != nil {
		return false, fmt.Errorf("check routed cohort membership: %w", err)
	}
	if present > 0 {
		return false, nil
	}

	facts, err := computeCanaryRunFacts(tx, runID)
	if err != nil {
		return false, err
	}
	facts.ChangedFiles, facts.ChangedLines = changedFiles, changedLines
	if err := insertCanaryCohortRun(tx, canaryCohortRouted, count, facts); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit canary intake: %w", err)
	}
	return true, nil
}

// GetCanaryReport projects the baseline and routed cohorts, their medians, and
// the advisory 30% target status. An unactivated canary reports Activated=false
// with empty cohorts.
func (d *DB) GetCanaryReport() (*CanaryReport, error) {
	report := &CanaryReport{TargetReduction: canaryTargetReduction}
	switch err := d.sql.QueryRow(`SELECT activated_at, fingerprint FROM canary_activation WHERE id = 1`).Scan(&report.ActivatedAt, &report.Fingerprint); err {
	case nil:
		report.Activated = true
	case sql.ErrNoRows:
		return report, nil
	default:
		return nil, fmt.Errorf("load canary activation: %w", err)
	}

	baseline, err := d.loadCanaryCohort(canaryCohortBaseline)
	if err != nil {
		return nil, err
	}
	routed, err := d.loadCanaryCohort(canaryCohortRouted)
	if err != nil {
		return nil, err
	}
	report.Baseline = baseline
	report.Routed = routed
	if len(baseline.Runs) > 0 && len(routed.Runs) > 0 {
		met := routed.MedianExecMS <= baseline.MedianExecMS*canaryRetainedPercent/100
		report.Met = &met
	}
	return report, nil
}

func (d *DB) loadCanaryCohort(cohort string) (CanaryCohort, error) {
	rows, err := d.sql.Query(`
		SELECT run_id, completed_at, execution_ms, invocation_ms, escalations, failovers, changed_files, changed_lines, initial_findings
		FROM canary_cohort_runs WHERE cohort = ? ORDER BY position`, cohort)
	if err != nil {
		return CanaryCohort{}, fmt.Errorf("load canary cohort %q: %w", cohort, err)
	}
	defer rows.Close()
	var c CanaryCohort
	var execs []int64
	for rows.Next() {
		var f CanaryRunFacts
		if err := rows.Scan(&f.RunID, &f.CompletedAt, &f.ExecutionMS, &f.InvocationMS, &f.Escalations, &f.Failovers, &f.ChangedFiles, &f.ChangedLines, &f.InitialFindings); err != nil {
			return CanaryCohort{}, fmt.Errorf("scan canary cohort run: %w", err)
		}
		c.Runs = append(c.Runs, f)
		execs = append(execs, f.ExecutionMS)
	}
	if err := rows.Err(); err != nil {
		return CanaryCohort{}, fmt.Errorf("iterate canary cohort %q: %w", cohort, err)
	}
	c.Complete = len(c.Runs) >= canaryCohortSize
	c.MedianExecMS = medianInt64(execs)
	return c, nil
}

func insertCanaryCohortRun(tx *sql.Tx, cohort string, position int, f *CanaryRunFacts) error {
	if _, err := tx.Exec(`
		INSERT INTO canary_cohort_runs
		  (cohort, position, run_id, completed_at, execution_ms, invocation_ms, escalations, failovers, changed_files, changed_lines, initial_findings, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cohort, position, f.RunID, f.CompletedAt, f.ExecutionMS, f.InvocationMS, f.Escalations, f.Failovers, f.ChangedFiles, f.ChangedLines, f.InitialFindings, now()); err != nil {
		return fmt.Errorf("insert canary cohort run: %w", err)
	}
	return nil
}

// computeCanaryRunFacts derives one run's frozen workload facts from durable
// pipeline records. Changed file/line counts are supplied by the caller.
func computeCanaryRunFacts(q canaryQueryer, runID string) (*CanaryRunFacts, error) {
	facts := &CanaryRunFacts{RunID: runID, ChangedFiles: -1, ChangedLines: -1}
	if err := q.QueryRow(`SELECT updated_at FROM runs WHERE id = ?`, runID).Scan(&facts.CompletedAt); err != nil {
		return nil, fmt.Errorf("load run completion time: %w", err)
	}
	// Execution-only agent-bearing Step-round metric: the wall-clock of step
	// rounds that actually launched an agent invocation.
	if err := q.QueryRow(`
		SELECT COALESCE(SUM(sr.duration_ms), 0)
		FROM step_rounds sr
		JOIN step_results s ON s.id = sr.step_result_id
		WHERE s.run_id = ?
		  AND EXISTS (SELECT 1 FROM invocation_attempt_starts ia WHERE ia.step_round_id = sr.id)`, runID).Scan(&facts.ExecutionMS); err != nil {
		return nil, fmt.Errorf("sum agent-bearing step rounds: %w", err)
	}
	if err := q.QueryRow(`
		SELECT COALESCE(SUM(t.duration_ms), 0)
		FROM invocation_attempt_terminals t
		JOIN invocation_attempt_starts ia ON ia.id = t.attempt_id
		WHERE ia.run_id = ?`, runID).Scan(&facts.InvocationMS); err != nil {
		return nil, fmt.Errorf("sum invocation durations: %w", err)
	}
	if err := q.QueryRow(`SELECT count(*) FROM invocation_attempt_starts WHERE run_id = ? AND tier > 0`, runID).Scan(&facts.Escalations); err != nil {
		return nil, fmt.Errorf("count escalations: %w", err)
	}
	if err := q.QueryRow(`SELECT count(*) FROM invocation_attempt_starts WHERE run_id = ? AND candidate_index > 0`, runID).Scan(&facts.Failovers); err != nil {
		return nil, fmt.Errorf("count failovers: %w", err)
	}
	var reviewFindings sql.NullString
	switch err := q.QueryRow(`SELECT findings_json FROM step_results WHERE run_id = ? AND step_name = ? LIMIT 1`, runID, types.StepReview).Scan(&reviewFindings); err {
	case nil, sql.ErrNoRows:
	default:
		return nil, fmt.Errorf("load review findings: %w", err)
	}
	if reviewFindings.Valid {
		facts.InitialFindings = findingsCount(&reviewFindings.String)
	}
	return facts, nil
}

func medianInt64(vals []int64) int64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]int64, n)
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

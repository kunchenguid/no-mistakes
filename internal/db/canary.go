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
	// Met is nil until both frozen cohorts contain ten successful runs.
	// Otherwise it reports whether the complete routed cohort met the advisory
	// target.
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

	var completionFence int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM run_completion_order`).Scan(&completionFence); err != nil {
		return false, fmt.Errorf("read canary completion fence: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO canary_activation (id, activated_at, fingerprint, completion_fence) VALUES (1, ?, ?, ?)`, now(), fingerprint, completionFence); err != nil {
		return false, fmt.Errorf("record canary activation: %w", err)
	}

	type baselineRun struct{ id, baseSHA, headSHA string }
	var baseline []baselineRun
	rows, err := tx.Query(`
		SELECT r.id, r.base_sha, r.head_sha
		FROM runs r
		JOIN run_completion_order c ON c.run_id = r.id
		WHERE r.status = ?
		ORDER BY c.sequence DESC
		LIMIT ?`, types.RunCompleted, canaryCohortSize)
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

// RecordRoutedRunInCanary offers a successful routed run to the durable
// first-ten cohort and backfills any earlier eligible completion whose intake
// was missed. Completion ordinals, rather than second-resolution timestamps or
// intake arrival order, exclude pre-activation runs and give concurrent
// completions a stable total order. Failed, cancelled, duplicate, pre-fence,
// and later successful runs are not themselves added. Returns true only when
// runID was added by this call.
func (d *DB) RecordRoutedRunInCanary(runID string, changedFiles, changedLines int) (bool, error) {
	return d.backfillRoutedCanary(runID, changedFiles, changedLines)
}

// backfillRoutedCanary reconstructs missing routed cohort members from the
// durable completion order. currentRunID is optional; when supplied, its
// changed-file facts are retained and the return value reports whether that run
// was inserted. Facts backfilled at a later boundary use the established -1
// marker for changed-file data that can no longer be recomputed safely.
func (d *DB) backfillRoutedCanary(currentRunID string, changedFiles, changedLines int) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("begin canary intake: %w", err)
	}
	defer tx.Rollback()

	var completionFence int64
	switch err := tx.QueryRow(`SELECT completion_fence FROM canary_activation WHERE id = 1`).Scan(&completionFence); err {
	case nil:
	case sql.ErrNoRows:
		return false, nil // dormant
	default:
		return false, fmt.Errorf("check canary activation: %w", err)
	}

	if currentRunID != "" {
		var status string
		var completionSequence sql.NullInt64
		switch err := tx.QueryRow(`
			SELECT r.status, c.sequence
			FROM runs r
			LEFT JOIN run_completion_order c ON c.run_id = r.id
			WHERE r.id = ?`, currentRunID).Scan(&status, &completionSequence); err {
		case nil:
		case sql.ErrNoRows:
			return false, nil
		default:
			return false, fmt.Errorf("load canary run: %w", err)
		}
		if types.RunStatus(status) != types.RunCompleted {
			return false, nil
		}
		if !completionSequence.Valid {
			return false, fmt.Errorf("completed canary run %s has no durable completion ordinal", currentRunID)
		}
		if completionSequence.Int64 <= completionFence {
			return false, nil
		}
	}

	var preFenceMembers int
	if err := tx.QueryRow(`
		SELECT count(*)
		FROM canary_cohort_runs cr
		JOIN run_completion_order c ON c.run_id = cr.run_id
		WHERE cr.cohort = ? AND c.sequence <= ?`,
		canaryCohortRouted, completionFence).Scan(&preFenceMembers); err != nil {
		return false, fmt.Errorf("count migrated routed canary members: %w", err)
	}
	remaining := canaryCohortSize - preFenceMembers
	if remaining <= 0 {
		return false, nil
	}

	rows, err := tx.Query(`
		SELECT r.id
		FROM run_completion_order c
		JOIN runs r ON r.id = c.run_id
		WHERE r.status = ? AND c.sequence > ?
		ORDER BY c.sequence
		LIMIT ?`, types.RunCompleted, completionFence, remaining)
	if err != nil {
		return false, fmt.Errorf("select routed canary completions: %w", err)
	}
	runIDs := make([]string, 0, remaining)
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan routed canary completion: %w", err)
		}
		runIDs = append(runIDs, runID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate routed canary completions: %w", err)
	}

	addedCurrent := false
	for rank, runID := range runIDs {
		var present int
		if err := tx.QueryRow(`SELECT count(*) FROM canary_cohort_runs WHERE cohort = ? AND run_id = ?`, canaryCohortRouted, runID).Scan(&present); err != nil {
			return false, fmt.Errorf("check routed cohort membership: %w", err)
		}
		if present > 0 {
			continue
		}
		facts, err := computeCanaryRunFacts(tx, runID)
		if err != nil {
			return false, err
		}
		if runID == currentRunID {
			facts.ChangedFiles, facts.ChangedLines = changedFiles, changedLines
			addedCurrent = true
		}
		if err := insertCanaryCohortRun(tx, canaryCohortRouted, preFenceMembers+rank, facts); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit canary intake: %w", err)
	}
	return addedCurrent, nil
}

// GetCanaryReport retries durable routed-cohort backfill, then projects the
// baseline and routed cohorts, their medians, and the advisory 30% target
// status. An unactivated canary reports Activated=false with empty cohorts.
func (d *DB) GetCanaryReport() (*CanaryReport, error) {
	if _, err := d.backfillRoutedCanary("", -1, -1); err != nil {
		return nil, fmt.Errorf("backfill routed canary report: %w", err)
	}
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
	if baseline.Complete && routed.Complete {
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
	switch err := q.QueryRow(`
		SELECT round.findings_json
		FROM step_rounds AS round
		JOIN step_results AS step ON step.id = round.step_result_id
		WHERE step.run_id = ?
		  AND step.step_name = ?
		  AND round.trigger_type = 'initial'
		  AND round.state = ?
		ORDER BY round.round ASC, round.created_at ASC, round.id ASC
		LIMIT 1`, runID, types.StepReview, StepRoundCompleted).Scan(&reviewFindings); err {
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

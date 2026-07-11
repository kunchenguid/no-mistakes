package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// Repair statuses track a repair cycle's terminal disposition.
const (
	RepairStatusPending     = "pending"
	RepairStatusResolved    = "resolved"
	RepairStatusUnresolved  = "unresolved"
	RepairStatusFailed      = "failed"
	RepairStatusUnavailable = "unavailable"
)

// Repair verdicts are the strong verifier's adjudication of a lineage. Only
// RepairVerdictResolved succeeds; every other verdict fails closed.
const (
	RepairVerdictResolved     = "resolved"
	RepairVerdictUnresolved   = "unresolved"
	RepairVerdictInconclusive = "inconclusive"
)

// FindingRepair is one fix→checks→verify cycle for a blocking finding lineage
// at one quality tier. The finding content, tier, and remaining budget are
// immutable once started; the fixer/verifier attempt links and the verdict and
// status are set as the cycle progresses. Escalation appends a new row per tier
// keyed by the same lineage_id.
type FindingRepair struct {
	ID                string
	RunID             string
	LineageID         string
	StepResultID      string
	StepRoundID       string
	Severity          string
	Action            string
	Description       string
	File              string
	Line              int
	Tier              int
	RemainingBudget   int
	FixerAttemptID    string
	VerifierAttemptID string
	Verdict           string
	VerdictRationale  string
	Status            string
	CreatedAt         int64
	UpdatedAt         int64
}

// FindingRepairCheck is one deterministic check run recorded against a repair
// cycle before its strong verifier.
type FindingRepairCheck struct {
	ID            string
	RepairID      string
	Command       string
	Applicable    bool
	ExitCode      int
	OutputExcerpt string
	RanAt         int64
}

// FindingRepairStart is the immutable content of a new repair cycle.
type FindingRepairStart struct {
	RunID           string
	LineageID       string
	StepResultID    string
	StepRoundID     string
	Severity        string
	Action          string
	Description     string
	File            string
	Line            int
	Tier            int
	RemainingBudget int
}

func (s FindingRepairStart) validate() error {
	switch {
	case strings.TrimSpace(s.RunID) == "":
		return fmt.Errorf("finding repair requires a run id")
	case strings.TrimSpace(s.LineageID) == "":
		return fmt.Errorf("finding repair requires a lineage id")
	case strings.TrimSpace(s.StepResultID) == "" || strings.TrimSpace(s.StepRoundID) == "":
		return fmt.Errorf("finding repair requires a step result and round")
	case strings.TrimSpace(s.Severity) == "" || strings.TrimSpace(s.Action) == "":
		return fmt.Errorf("finding repair requires severity and action")
	case strings.TrimSpace(s.Description) == "":
		return fmt.Errorf("finding repair requires the finding description")
	case s.Tier < 0:
		return fmt.Errorf("finding repair tier must be non-negative")
	case s.RemainingBudget < 0:
		return fmt.Errorf("finding repair remaining budget must be non-negative")
	}
	return nil
}

// StartFindingRepair persists a new repair cycle with immutable finding content.
func (d *DB) StartFindingRepair(s FindingRepairStart) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	id := newID()
	ts := now()
	_, err := d.sql.Exec(
		`INSERT INTO finding_repairs (id, run_id, lineage_id, step_result_id, step_round_id, severity, action, description, file, line, tier, remaining_budget, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, s.RunID, s.LineageID, s.StepResultID, s.StepRoundID, s.Severity, s.Action, s.Description,
		nullIfEmpty(s.File), nullLine(s.Line), s.Tier, s.RemainingBudget, RepairStatusPending, ts, ts,
	)
	if err != nil {
		return "", fmt.Errorf("insert finding repair: %w", err)
	}
	return id, nil
}

// SetFindingRepairFixer links the fixer invocation attempt to a repair cycle.
func (d *DB) SetFindingRepairFixer(repairID, fixerAttemptID string) error {
	if strings.TrimSpace(repairID) == "" || strings.TrimSpace(fixerAttemptID) == "" {
		return fmt.Errorf("link finding repair fixer requires repair and attempt ids")
	}
	result, err := d.sql.Exec(`UPDATE finding_repairs SET fixer_attempt_id = ?, updated_at = ? WHERE id = ?`, fixerAttemptID, now(), repairID)
	if err != nil {
		return fmt.Errorf("link finding repair fixer: %w", err)
	}
	return requireFindingRepairUpdate(result, repairID, "link fixer")
}

// SetFindingRepairVerifier links the strong verifier invocation attempt.
func (d *DB) SetFindingRepairVerifier(repairID, verifierAttemptID string) error {
	if strings.TrimSpace(repairID) == "" || strings.TrimSpace(verifierAttemptID) == "" {
		return fmt.Errorf("link finding repair verifier requires repair and attempt ids")
	}
	result, err := d.sql.Exec(`UPDATE finding_repairs SET verifier_attempt_id = ?, updated_at = ? WHERE id = ?`, verifierAttemptID, now(), repairID)
	if err != nil {
		return fmt.Errorf("link finding repair verifier: %w", err)
	}
	return requireFindingRepairUpdate(result, repairID, "link verifier")
}

// RecordFindingRepairCheck appends one deterministic check run to a repair cycle.
func (d *DB) RecordFindingRepairCheck(repairID, command string, applicable bool, exitCode int, outputExcerpt string) error {
	if _, err := d.sql.Exec(
		`INSERT INTO finding_repair_checks (id, repair_id, command, applicable, exit_code, output_excerpt, ran_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		newID(), repairID, command, boolToInt(applicable), exitCode, nullIfEmpty(outputExcerpt), now(),
	); err != nil {
		return fmt.Errorf("record finding repair check: %w", err)
	}
	return nil
}

// ResolveFindingRepair records the verifier's verdict, rationale, and the
// repair cycle's terminal status.
func (d *DB) ResolveFindingRepair(repairID, verdict, rationale, status string) error {
	switch verdict {
	case RepairVerdictResolved, RepairVerdictUnresolved, RepairVerdictInconclusive:
	default:
		return fmt.Errorf("resolve finding repair requires a valid verdict")
	}
	switch status {
	case RepairStatusResolved, RepairStatusUnresolved, RepairStatusFailed, RepairStatusUnavailable:
	default:
		return fmt.Errorf("resolve finding repair requires a terminal status")
	}
	result, err := d.sql.Exec(
		`UPDATE finding_repairs SET verdict = ?, verdict_rationale = ?, status = ?, updated_at = ? WHERE id = ?`,
		verdict, nullIfEmpty(rationale), status, now(), repairID,
	)
	if err != nil {
		return fmt.Errorf("resolve finding repair: %w", err)
	}
	return requireFindingRepairUpdate(result, repairID, "resolve")
}

func requireFindingRepairUpdate(result sql.Result, repairID, operation string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s finding repair rows affected: %w", operation, err)
	}
	if changed != 1 {
		return fmt.Errorf("%s finding repair %q: not found", operation, repairID)
	}
	return nil
}

// GetFindingRepairsByRun returns a run's repair cycles in creation order.
func (d *DB) GetFindingRepairsByRun(runID string) ([]*FindingRepair, error) {
	return d.queryFindingRepairs(`WHERE run_id = ? ORDER BY created_at, id`, runID)
}

// GetFindingRepairsByLineage returns a lineage's repair cycles in tier order.
func (d *DB) GetFindingRepairsByLineage(lineageID string) ([]*FindingRepair, error) {
	return d.queryFindingRepairs(`WHERE lineage_id = ? ORDER BY tier, created_at, id`, lineageID)
}

const findingRepairColumns = `id, run_id, lineage_id, step_result_id, step_round_id, severity, action, description, file, line, tier, remaining_budget, fixer_attempt_id, verifier_attempt_id, verdict, verdict_rationale, status, created_at, updated_at`

func (d *DB) queryFindingRepairs(whereOrder string, args ...any) ([]*FindingRepair, error) {
	rows, err := d.sql.Query(`SELECT `+findingRepairColumns+` FROM finding_repairs `+whereOrder, args...)
	if err != nil {
		return nil, fmt.Errorf("query finding repairs: %w", err)
	}
	defer rows.Close()
	var repairs []*FindingRepair
	for rows.Next() {
		repair, err := scanFindingRepair(rows)
		if err != nil {
			return nil, err
		}
		repairs = append(repairs, repair)
	}
	return repairs, rows.Err()
}

func scanFindingRepair(row interface{ Scan(...any) error }) (*FindingRepair, error) {
	var (
		r         FindingRepair
		file      sql.NullString
		line      sql.NullInt64
		fixer     sql.NullString
		verifier  sql.NullString
		verdict   sql.NullString
		rationale sql.NullString
	)
	if err := row.Scan(&r.ID, &r.RunID, &r.LineageID, &r.StepResultID, &r.StepRoundID, &r.Severity, &r.Action, &r.Description,
		&file, &line, &r.Tier, &r.RemainingBudget, &fixer, &verifier, &verdict, &rationale, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scan finding repair: %w", err)
	}
	r.File = file.String
	r.Line = int(line.Int64)
	r.FixerAttemptID = fixer.String
	r.VerifierAttemptID = verifier.String
	r.Verdict = verdict.String
	r.VerdictRationale = rationale.String
	return &r, nil
}

// GetFindingRepairChecks returns a repair cycle's deterministic check runs in order.
func (d *DB) GetFindingRepairChecks(repairID string) ([]FindingRepairCheck, error) {
	rows, err := d.sql.Query(`SELECT id, repair_id, command, applicable, exit_code, output_excerpt, ran_at FROM finding_repair_checks WHERE repair_id = ? ORDER BY ran_at, id`, repairID)
	if err != nil {
		return nil, fmt.Errorf("query finding repair checks: %w", err)
	}
	defer rows.Close()
	var checks []FindingRepairCheck
	for rows.Next() {
		var (
			c          FindingRepairCheck
			applicable int
			excerpt    sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.RepairID, &c.Command, &applicable, &c.ExitCode, &excerpt, &c.RanAt); err != nil {
			return nil, fmt.Errorf("scan finding repair check: %w", err)
		}
		c.Applicable = applicable != 0
		c.OutputExcerpt = excerpt.String
		checks = append(checks, c)
	}
	return checks, rows.Err()
}

func nullLine(line int) any {
	if line <= 0 {
		return nil
	}
	return line
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// HasUnresolvedBlockingRepair reports whether any blocking (error or warning)
// finding lineage in the run ended its repair cascade without a resolved
// verdict — exhausted or inconclusive. Unattended consent must fail rather than
// approve when this is true.
func (d *DB) HasUnresolvedBlockingRepair(runID string) (bool, error) {
	repairs, err := d.GetFindingRepairsByRun(runID)
	if err != nil {
		return false, err
	}
	// Keep only the latest repair per blocking lineage; that row carries the
	// lineage's terminal disposition.
	latest := make(map[string]*FindingRepair)
	for _, r := range repairs {
		if r.Severity != "error" && r.Severity != "warning" {
			continue
		}
		cur, ok := latest[r.LineageID]
		if !ok || r.CreatedAt > cur.CreatedAt || (r.CreatedAt == cur.CreatedAt && r.ID > cur.ID) {
			latest[r.LineageID] = r
		}
	}
	for _, r := range latest {
		if r.Status != RepairStatusResolved {
			return true, nil
		}
	}
	return false, nil
}

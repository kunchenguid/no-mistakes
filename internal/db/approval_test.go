package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	testGateFindings     = `{"summary":"one issue","findings":[{"id":"finding-1"}]}`
	testSelectedFindings = `["finding-1"]`
	testInstructions     = `{"finding-1":"preserve the public API"}`
	testAddedFindings    = `[{"id":"user-1","description":"also cover rollback"}]`
)

type approvalFixture struct {
	d     *DB
	runID string
	step  *StepResult
	round *StepRound
	gate  *ApprovalGate
}

func newApprovalFixture(t *testing.T) approvalFixture {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/approval-repo", "git@github.com:test/approval.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature/approval", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("start run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	findings := testGateFindings
	round, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 31)
	if err != nil {
		t.Fatalf("insert round: %v", err)
	}
	return approvalFixture{d: d, runID: run.ID, step: step, round: round}
}

func (f *approvalFixture) park(t *testing.T) *ApprovalGate {
	t.Helper()
	gate, err := f.d.ParkApprovalGate(ParkApprovalGateInput{
		RunID:         f.runID,
		StepResultID:  f.step.ID,
		SourceRoundID: f.round.ID,
		Status:        types.StepStatusAwaitingApproval,
		FindingsJSON:  testGateFindings,
		DurationMS:    317,
	})
	if err != nil {
		t.Fatalf("park approval gate: %v", err)
	}
	f.gate = gate
	return gate
}

func validApprovalActionInput(f approvalFixture) ApprovalActionInput {
	return ApprovalActionInput{
		GateID:                 f.gate.ID,
		RunID:                  f.runID,
		StepResultID:           f.step.ID,
		StepRoundID:            f.round.ID,
		Action:                 types.ActionFix,
		SelectedFindingIDsJSON: testSelectedFindings,
		InstructionsJSON:       testInstructions,
		AddedFindingsJSON:      testAddedFindings,
	}
}

func applyApprovalFixtureFix(f approvalFixture, actionID string, parkedMS int64) error {
	selected := testSelectedFindings
	return f.d.ApplyApprovalFix(ApplyApprovalFixInput{ActionID: actionID, ParkedMS: parkedMS, SelectedIDsJSON: &selected})
}

func countRows(t *testing.T, d *DB, table string) int {
	t.Helper()
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func installFailTrigger(t *testing.T, d *DB, name, timing string) {
	t.Helper()
	_, err := d.sql.Exec("CREATE TRIGGER " + name + " BEFORE " + timing + " BEGIN SELECT RAISE(FAIL, 'injected " + name + " failure'); END")
	if err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
}

func assertUnchangedBeforePark(t *testing.T, f approvalFixture) {
	t.Helper()
	var status string
	var duration, findings, gateID any
	if err := f.d.sql.QueryRow(`SELECT status, duration_ms, findings_json, approval_gate_id FROM step_results WHERE id = ?`, f.step.ID).Scan(&status, &duration, &findings, &gateID); err != nil {
		t.Fatalf("query step after rollback: %v", err)
	}
	if status != string(types.StepStatusRunning) || duration != nil || findings != nil || gateID != nil {
		t.Fatalf("step mutated after rollback: status=%q duration=%v findings=%v gate=%v", status, duration, findings, gateID)
	}
	var awaiting any
	if err := f.d.sql.QueryRow(`SELECT awaiting_agent_since FROM runs WHERE id = ?`, f.runID).Scan(&awaiting); err != nil {
		t.Fatalf("query run after rollback: %v", err)
	}
	if awaiting != nil {
		t.Fatalf("run marker mutated after rollback: %v", awaiting)
	}
	if got := countRows(t, f.d, "approval_gates"); got != 0 {
		t.Fatalf("approval gate rows = %d, want 0", got)
	}
}

func TestApprovalSchemaMigratesStepGateIdentity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE step_results (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create legacy step_results: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("migrate legacy db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if !hasColumn(t, d, "step_results", "approval_gate_id") {
		t.Fatal("migrated step_results.approval_gate_id is missing")
	}
	for _, table := range []string{"approval_gates", "approval_actions"} {
		if got := countRows(t, d, table); got != 0 {
			t.Fatalf("%s rows = %d, want empty migrated table", table, got)
		}
	}
}

func TestParkApprovalGateAtomicallyPersistsGateStepAndRun(t *testing.T) {
	f := newApprovalFixture(t)
	gate := f.park(t)
	if gate.ID == "" || gate.RunID != f.runID || gate.StepResultID != f.step.ID || gate.SourceRoundID != f.round.ID {
		t.Fatalf("unexpected gate identity: %+v", gate)
	}
	if gate.Status != types.StepStatusAwaitingApproval || gate.FindingsJSON != testGateFindings || gate.DurationMS != 317 {
		t.Fatalf("gate facts not exact: %+v", gate)
	}
	currentGate, err := f.d.GetCurrentApprovalGate(f.step.ID)
	if err != nil {
		t.Fatalf("get current approval gate: %v", err)
	}
	storedGate, err := f.d.GetApprovalGate(gate.ID)
	if err != nil {
		t.Fatalf("get approval gate by ID: %v", err)
	}
	if currentGate == nil || storedGate == nil || *currentGate != *gate || *storedGate != *gate {
		t.Fatalf("durable gate lookup current=%+v stored=%+v want=%+v", currentGate, storedGate, gate)
	}

	var status, findings, gateID string
	var duration int64
	if err := f.d.sql.QueryRow(`SELECT status, findings_json, duration_ms, approval_gate_id FROM step_results WHERE id = ?`, f.step.ID).Scan(&status, &findings, &duration, &gateID); err != nil {
		t.Fatalf("query parked step: %v", err)
	}
	if status != string(gate.Status) || findings != testGateFindings || duration != 317 || gateID != gate.ID {
		t.Fatalf("parked step = (%q, %q, %d, %q), want exact gate facts", status, findings, duration, gateID)
	}
	var awaiting sql.NullInt64
	if err := f.d.sql.QueryRow(`SELECT awaiting_agent_since FROM runs WHERE id = ?`, f.runID).Scan(&awaiting); err != nil {
		t.Fatalf("query parked run: %v", err)
	}
	if !awaiting.Valid {
		t.Fatal("run awaiting marker was not set")
	}
}

func TestParkApprovalGateRollsBackWhenStepUpdateFails(t *testing.T) {
	f := newApprovalFixture(t)
	installFailTrigger(t, f.d, "fail_approval_step_update", "UPDATE OF approval_gate_id ON step_results")
	_, err := f.d.ParkApprovalGate(ParkApprovalGateInput{
		RunID: f.runID, StepResultID: f.step.ID, SourceRoundID: f.round.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: testGateFindings, DurationMS: 317,
	})
	if err == nil || !strings.Contains(err.Error(), "injected fail_approval_step_update failure") {
		t.Fatalf("park error = %v, want injected step failure", err)
	}
	assertUnchangedBeforePark(t, f)
}

func TestParkApprovalGateRollsBackWhenRunMarkerUpdateFails(t *testing.T) {
	f := newApprovalFixture(t)
	installFailTrigger(t, f.d, "fail_approval_run_park", "UPDATE OF awaiting_agent_since ON runs")
	_, err := f.d.ParkApprovalGate(ParkApprovalGateInput{
		RunID: f.runID, StepResultID: f.step.ID, SourceRoundID: f.round.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: testGateFindings, DurationMS: 317,
	})
	if err == nil || !strings.Contains(err.Error(), "injected fail_approval_run_park failure") {
		t.Fatalf("park error = %v, want injected run failure", err)
	}
	assertUnchangedBeforePark(t, f)
}

func TestParkApprovalGateCreatesDistinctIdentityForRepeatedStepGate(t *testing.T) {
	f := newApprovalFixture(t)
	firstGate := f.park(t)
	firstAction, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err != nil {
		t.Fatalf("insert first action: %v", err)
	}
	if err := applyApprovalFixtureFix(f, firstAction.ID, 11); err != nil {
		t.Fatalf("apply first fix: %v", err)
	}
	secondGate, err := f.d.ParkApprovalGate(ParkApprovalGateInput{
		RunID: f.runID, StepResultID: f.step.ID, SourceRoundID: f.round.ID,
		Status: types.StepStatusFixReview, FindingsJSON: testGateFindings, DurationMS: 401,
	})
	if err != nil {
		t.Fatalf("park repeated gate: %v", err)
	}
	if secondGate.ID == firstGate.ID {
		t.Fatalf("repeated gate reused identity %q", secondGate.ID)
	}
	var currentGateID string
	if err := f.d.sql.QueryRow(`SELECT approval_gate_id FROM step_results WHERE id = ?`, f.step.ID).Scan(&currentGateID); err != nil {
		t.Fatalf("query repeated current gate: %v", err)
	}
	if currentGateID != secondGate.ID || countRows(t, f.d, "approval_gates") != 2 {
		t.Fatalf("repeated gate current=%q rows=%d, want %q and 2", currentGateID, countRows(t, f.d, "approval_gates"), secondGate.ID)
	}
}

func TestInsertApprovalActionPayloadRoundTripAndPendingLookup(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	input := validApprovalActionInput(f)
	action, err := f.d.InsertApprovalAction(input)
	if err != nil {
		t.Fatalf("insert approval action: %v", err)
	}
	if action.ID == "" || action.GateID != input.GateID || action.RunID != input.RunID || action.StepResultID != input.StepResultID || action.StepRoundID != input.StepRoundID || action.Action != input.Action {
		t.Fatalf("unexpected action identity: %+v", action)
	}
	if action.SelectedFindingIDsJSON != testSelectedFindings || action.InstructionsJSON != testInstructions || action.AddedFindingsJSON != testAddedFindings || action.AppliedAt != nil {
		t.Fatalf("action payload not preserved exactly: %+v", action)
	}
	var dbPath string
	if err := f.d.sql.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath); err != nil {
		t.Fatalf("locate approval database: %v", err)
	}
	if err := f.d.Close(); err != nil {
		t.Fatalf("close approval database before recovery: %v", err)
	}
	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen approval database: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.d = reopened

	pending, err := f.d.GetPendingApprovalAction(f.gate.ID)
	if err != nil {
		t.Fatalf("get pending action: %v", err)
	}
	if pending == nil || *pending != *action {
		t.Fatalf("pending action = %+v, want %+v", pending, action)
	}
	missing, err := f.d.GetPendingApprovalAction("missing-gate")
	if err != nil || missing != nil {
		t.Fatalf("missing pending action = (%+v, %v), want nil, nil", missing, err)
	}
}

func TestInsertApprovalActionAcceptsExactNullEmptyPayload(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	input := ApprovalActionInput{
		GateID: f.gate.ID, RunID: f.runID, StepResultID: f.step.ID, StepRoundID: f.round.ID,
		Action: types.ActionApprove, SelectedFindingIDsJSON: `null`, InstructionsJSON: `null`, AddedFindingsJSON: `null`,
	}
	action, err := f.d.InsertApprovalAction(input)
	if err != nil {
		t.Fatalf("insert approve action with null empty payload: %v", err)
	}
	got, err := f.d.GetPendingApprovalAction(f.gate.ID)
	if err != nil {
		t.Fatalf("get approve action: %v", err)
	}
	if got == nil || got.ID != action.ID || got.SelectedFindingIDsJSON != `null` || got.InstructionsJSON != `null` || got.AddedFindingsJSON != `null` {
		t.Fatalf("null payload not preserved exactly: %+v", got)
	}
}

func TestInsertApprovalActionRejectsInvalidOrStaleInputsWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, f approvalFixture, input *ApprovalActionInput)
	}{
		{name: "invalid action", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.Action = "retry" }},
		{name: "fix without selected or added finding", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) {
			in.SelectedFindingIDsJSON = `[]`
			in.AddedFindingsJSON = `[]`
		}},
		{name: "non-fix with selected finding", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.Action = types.ActionApprove }},
		{name: "malformed selected JSON", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.SelectedFindingIDsJSON = `[` }},
		{name: "malformed instructions JSON", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.InstructionsJSON = `[]` }},
		{name: "malformed added findings JSON", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.AddedFindingsJSON = `{}` }},
		{name: "mismatched run", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.RunID = "different-run" }},
		{name: "mismatched step", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.StepResultID = "different-step" }},
		{name: "mismatched source round", mutate: func(_ *testing.T, _ approvalFixture, in *ApprovalActionInput) { in.StepRoundID = "different-round" }},
		{name: "stale gate", mutate: func(t *testing.T, f approvalFixture, _ *ApprovalActionInput) {
			if _, err := f.d.sql.Exec(`UPDATE step_results SET approval_gate_id = NULL WHERE id = ?`, f.step.ID); err != nil {
				t.Fatalf("make gate stale: %v", err)
			}
		}},
		{name: "wrong current gate status", mutate: func(t *testing.T, f approvalFixture, _ *ApprovalActionInput) {
			if _, err := f.d.sql.Exec(`UPDATE step_results SET status = ? WHERE id = ?`, types.StepStatusRunning, f.step.ID); err != nil {
				t.Fatalf("change gate status: %v", err)
			}
		}},
		{name: "unparked run", mutate: func(t *testing.T, f approvalFixture, _ *ApprovalActionInput) {
			if _, err := f.d.sql.Exec(`UPDATE runs SET awaiting_agent_since = NULL WHERE id = ?`, f.runID); err != nil {
				t.Fatalf("clear parked marker: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newApprovalFixture(t)
			f.park(t)
			input := validApprovalActionInput(f)
			tt.mutate(t, f, &input)
			if action, err := f.d.InsertApprovalAction(input); err == nil || action != nil {
				t.Fatalf("insert approval action = (%+v, %v), want nil error", action, err)
			}
			if got := countRows(t, f.d, "approval_actions"); got != 0 {
				t.Fatalf("approval action rows = %d, want 0", got)
			}
		})
	}
}

func TestInsertApprovalActionRollsBackOnWriteFailure(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	installFailTrigger(t, f.d, "fail_approval_action_insert", "INSERT ON approval_actions")
	action, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err == nil || action != nil || !strings.Contains(err.Error(), "injected fail_approval_action_insert failure") {
		t.Fatalf("insert approval action = (%+v, %v), want injected failure", action, err)
	}
	if got := countRows(t, f.d, "approval_actions"); got != 0 {
		t.Fatalf("approval action rows = %d, want 0", got)
	}
}

func TestInsertApprovalActionRejectsDuplicateWithoutMutation(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	first, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err != nil {
		t.Fatalf("insert first action: %v", err)
	}
	duplicate := validApprovalActionInput(f)
	duplicate.InstructionsJSON = `{"finding-1":"different"}`
	if action, err := f.d.InsertApprovalAction(duplicate); err == nil || action != nil {
		t.Fatalf("duplicate insert = (%+v, %v), want rejection", action, err)
	}
	if got := countRows(t, f.d, "approval_actions"); got != 1 {
		t.Fatalf("approval action rows = %d, want 1", got)
	}
	got, err := f.d.GetPendingApprovalAction(f.gate.ID)
	if err != nil {
		t.Fatalf("get original action: %v", err)
	}
	if got == nil || got.ID != first.ID || got.InstructionsJSON != testInstructions {
		t.Fatalf("original action mutated: %+v", got)
	}
}

func TestApprovalActionPayloadCannotBeUpdated(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	action, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	if _, err := f.d.sql.Exec(`UPDATE approval_actions SET instructions_json = '{}' WHERE id = ?`, action.ID); err == nil || !strings.Contains(err.Error(), "approval action payload is immutable") {
		t.Fatalf("payload update error = %v, want immutable rejection", err)
	}
	got, err := f.d.GetPendingApprovalAction(f.gate.ID)
	if err != nil {
		t.Fatalf("get action after rejected update: %v", err)
	}
	if got == nil || got.InstructionsJSON != testInstructions {
		t.Fatalf("payload mutated after rejected update: %+v", got)
	}
}

func TestApplyApprovalFixAtomicallyAppliesAndClearsMarker(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	action, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	if err := applyApprovalFixtureFix(f, action.ID, 47); err != nil {
		t.Fatalf("apply fix: %v", err)
	}
	var applied sql.NullInt64
	if err := f.d.sql.QueryRow(`SELECT applied_at FROM approval_actions WHERE id = ?`, action.ID).Scan(&applied); err != nil {
		t.Fatalf("query applied action: %v", err)
	}
	if !applied.Valid {
		t.Fatal("action applied_at was not set")
	}
	var awaiting sql.NullInt64
	var parked int64
	if err := f.d.sql.QueryRow(`SELECT awaiting_agent_since, COALESCE(parked_ms, 0) FROM runs WHERE id = ?`, f.runID).Scan(&awaiting, &parked); err != nil {
		t.Fatalf("query resumed run: %v", err)
	}
	if awaiting.Valid || parked != 47 {
		t.Fatalf("run after completion = (awaiting=%v, parked=%d), want cleared and 47", awaiting, parked)
	}
	pending, err := f.d.GetPendingApprovalAction(f.gate.ID)
	if err != nil || pending != nil {
		t.Fatalf("pending after completion = (%+v, %v), want nil, nil", pending, err)
	}

	if err := applyApprovalFixtureFix(f, action.ID, 999); err != nil {
		t.Fatalf("repeat fix application: %v", err)
	}
	var appliedAgain int64
	if err := f.d.sql.QueryRow(`SELECT applied_at FROM approval_actions WHERE id = ?`, action.ID).Scan(&appliedAgain); err != nil {
		t.Fatalf("query repeated completion: %v", err)
	}
	if appliedAgain != applied.Int64 {
		t.Fatalf("applied_at changed on repeat: got %d, want %d", appliedAgain, applied.Int64)
	}
	if err := f.d.sql.QueryRow(`SELECT COALESCE(parked_ms, 0) FROM runs WHERE id = ?`, f.runID).Scan(&parked); err != nil {
		t.Fatalf("query parked duration after repeat: %v", err)
	}
	if parked != 47 {
		t.Fatalf("parked duration after repeat = %d, want 47", parked)
	}
}

func TestApplyApprovalFixRollsBackWhenAppliedUpdateFails(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	action, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	installFailTrigger(t, f.d, "fail_approval_applied_update", "UPDATE OF applied_at ON approval_actions")
	if err := applyApprovalFixtureFix(f, action.ID, 47); err == nil || !strings.Contains(err.Error(), "injected fail_approval_applied_update failure") {
		t.Fatalf("apply error = %v, want injected action failure", err)
	}
	assertActionStillPendingAndParked(t, f, action.ID)
}

func TestApplyApprovalFixRollsBackWhenMarkerClearFails(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	action, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	installFailTrigger(t, f.d, "fail_approval_marker_clear", "UPDATE OF awaiting_agent_since ON runs")
	if err := applyApprovalFixtureFix(f, action.ID, 47); err == nil || !strings.Contains(err.Error(), "injected fail_approval_marker_clear failure") {
		t.Fatalf("apply error = %v, want injected marker failure", err)
	}
	assertActionStillPendingAndParked(t, f, action.ID)
}

func TestApplyApprovalFixRejectsUnparkedGateWithoutApplying(t *testing.T) {
	f := newApprovalFixture(t)
	f.park(t)
	action, err := f.d.InsertApprovalAction(validApprovalActionInput(f))
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	if _, err := f.d.sql.Exec(`UPDATE runs SET awaiting_agent_since = NULL WHERE id = ?`, f.runID); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if err := applyApprovalFixtureFix(f, action.ID, 47); err == nil {
		t.Fatal("apply unparked action succeeded")
	}
	var applied any
	if err := f.d.sql.QueryRow(`SELECT applied_at FROM approval_actions WHERE id = ?`, action.ID).Scan(&applied); err != nil {
		t.Fatalf("query rejected completion: %v", err)
	}
	if applied != nil {
		t.Fatalf("unparked rejected completion set applied_at=%v", applied)
	}
}

func assertActionStillPendingAndParked(t *testing.T, f approvalFixture, actionID string) {
	t.Helper()
	var applied any
	if err := f.d.sql.QueryRow(`SELECT applied_at FROM approval_actions WHERE id = ?`, actionID).Scan(&applied); err != nil {
		t.Fatalf("query action after rollback: %v", err)
	}
	if applied != nil {
		t.Fatalf("action applied_at mutated after rollback: %v", applied)
	}
	var awaiting sql.NullInt64
	var parked int64
	if err := f.d.sql.QueryRow(`SELECT awaiting_agent_since, COALESCE(parked_ms, 0) FROM runs WHERE id = ?`, f.runID).Scan(&awaiting, &parked); err != nil {
		t.Fatalf("query run after rollback: %v", err)
	}
	if !awaiting.Valid || parked != 0 {
		t.Fatalf("run mutated after rollback: awaiting=%v parked=%d", awaiting, parked)
	}
	pending, err := f.d.GetPendingApprovalAction(f.gate.ID)
	if err != nil || pending == nil || pending.ID != actionID {
		t.Fatalf("pending action after rollback = (%+v, %v)", pending, err)
	}
}

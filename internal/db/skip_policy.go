package db

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RequestedSkips decodes the immutable launch policy. Nil means legacy, not
// evidence of an empty policy. Malformed policy never becomes a full pipeline.
func (r *Run) RequestedSkips() ([]types.StepName, error) {
	if r.SkipStepsJSON == nil {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(*r.SkipStepsJSON), &names); err != nil || names == nil {
		return nil, fmt.Errorf("run %s has malformed skip policy", r.ID)
	}
	result := make([]types.StepName, 0, len(names))
	seen := map[types.StepName]bool{}
	for _, name := range names {
		step := types.StepName(name)
		if !types.IsCoreStepName(step) || seen[step] {
			return nil, fmt.Errorf("run %s has invalid skip policy entry %q", r.ID, name)
		}
		seen[step] = true
		result = append(result, step)
	}
	return result, nil
}

// SetRunSkippedSteps pins launch scope before execution. No active run can be
// tightened or widened, even if no step has started yet.
func (d *DB) SetRunSkippedSteps(runID string, steps []types.StepName) error {
	names := make([]types.StepName, 0, len(steps))
	seen := map[types.StepName]bool{}
	for _, step := range steps {
		if !types.IsCoreStepName(step) {
			return fmt.Errorf("invalid skipped step %q", step)
		}
		if !seen[step] {
			names = append(names, step)
			seen[step] = true
		}
	}
	raw, err := json.Marshal(names)
	if err != nil {
		return err
	}
	result, err := d.sql.Exec(`UPDATE runs SET skip_steps = ? WHERE id = ? AND skip_steps IS NULL AND status = 'pending' AND NOT EXISTS (SELECT 1 FROM step_results WHERE run_id = ?)`, string(raw), runID, runID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("run %s skip policy is already pinned or execution started", runID)
	}
	return nil
}

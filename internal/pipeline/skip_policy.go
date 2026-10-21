package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (e *Executor) loadSkipPolicy(run *db.Run, starting bool) error {
	stored, err := e.db.GetRun(run.ID)
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("run %s is not recorded", run.ID)
	}
	if stored.SkipStepsJSON == nil && starting {
		var requested []types.StepName
		if e.retrySource != "" {
			source, err := e.db.GetRun(e.retrySource)
			if err != nil {
				return err
			}
			if source == nil {
				return fmt.Errorf("retry source %s is not recorded", e.retrySource)
			}
			requested, err = source.RequestedSkips()
			if err != nil {
				return err
			}
		}
		for _, step := range types.AllSteps() {
			if e.skips[step] {
				requested = append(requested, step)
			}
		}
		if err := e.db.SetRunSkippedSteps(run.ID, requested); err != nil {
			return err
		}
		stored, err = e.db.GetRun(run.ID)
		if err != nil {
			return err
		}
	}
	skips, err := stored.RequestedSkips()
	if err != nil {
		return err
	}
	run.SkipStepsJSON = stored.SkipStepsJSON
	if stored.SkipStepsJSON != nil {
		e.SetSkippedSteps(skips)
	}
	return nil
}

// requireGateSkipPolicy runs under e.mu, before a response writes any decision.
// The parked step supplies the exact run identity, never a branch lookup.
func (e *Executor) requireGateSkipPolicy(runID string, required []types.StepName) error {
	if e.db == nil || runID == "" || len(required) == 0 {
		return fmt.Errorf("scoped response requires an exact run and required skips")
	}
	step, err := e.db.GetStepResult(e.waitingStepResultID)
	if err != nil {
		return err
	}
	if step == nil || step.RunID != runID {
		return fmt.Errorf("scoped response does not match the parked run")
	}
	if e.skips[step.StepName] {
		return fmt.Errorf("parked approval gate contradicts skip policy")
	}
	run, err := e.db.GetRun(runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("run %s is not recorded", runID)
	}
	skips, err := run.RequestedSkips()
	if err != nil {
		return err
	}
	if err := types.RequireSkippedSteps(skips, run.SkipStepsJSON != nil, required); err != nil {
		return err
	}
	for _, skip := range skips {
		if !e.skips[skip] {
			return fmt.Errorf("executor does not enforce persisted skip %q", skip)
		}
	}
	return nil
}

package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// lateCIMonitor exists only while the CI step is executing a monitoring round.
// Cancellation is joined by executeInterruptibleCI before any repair executes.
type lateCIMonitor struct {
	runID, stepID string
	cancel        context.CancelFunc
	response      *approvalResponse
	findings      string
	done          chan error
	invalidate    func()
	verify        func(context.Context) error
}

// RespondToLateCIFinding is the exact-head-bound amendment path for an active
// CI monitor. It preserves the run, worktree, push generation and PR identity.
// A durable gate precedes cancellation, so a daemon crash retains the finding
// for an explicit response instead of restoring stale readiness.
func (e *Executor) RespondToLateCIFinding(runID string, step types.StepName, action types.ApprovalAction, ids []string, instructions map[string]string, added []types.Finding, reason, head string) error {
	e.mu.Lock()
	err := e.admitLateCIFinding(runID, step, action, ids, instructions, added, reason, head)
	var done chan error
	if err == nil {
		done = e.lateCI.done
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("late finding retained; monitor handoff still settling; inspect axi status")
	}
}

func (e *Executor) admitLateCIFinding(runID string, step types.StepName, action types.ApprovalAction, ids []string, instructions map[string]string, added []types.Finding, reason, head string) error {
	if step != types.StepCI || action != types.ActionFix || len(added) == 0 || len(ids) != 0 || reason != "" {
		return fmt.Errorf("--head requires --step ci --action fix --add-finding, without existing finding IDs or an approval reason")
	}
	for _, finding := range added {
		if finding.Description == "" {
			return fmt.Errorf("late CI finding requires a description")
		}
	}
	monitor := e.lateCI
	if e.waiting || monitor == nil || monitor.response != nil || monitor.runID != runID {
		return fmt.Errorf("late findings require a CI monitoring execution after validation and publication, outside a fix execution; inspect axi status")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	live, err := git.HeadSHA(ctx, e.workDir)
	if err != nil || live != head {
		return fmt.Errorf("late CI finding head does not match the managed worktree")
	}
	if monitor.verify == nil {
		return fmt.Errorf("CI monitor cannot verify live PR lifecycle")
	}
	if err := monitor.verify(ctx); err != nil {
		return err
	}
	// User findings are persisted as ask-user, preventing crash recovery from
	// automatically applying an amendment whose response was interrupted.
	persisted := append([]types.Finding(nil), added...)
	for i := range persisted {
		persisted[i].Action = types.ActionAskUser
		persisted[i].Category = types.FindingCategoryCILateFinding
		if note, ok := instructions[persisted[i].ID]; ok {
			persisted[i].UserInstructions = note
		}
	}
	raw, err := json.Marshal(types.Findings{Items: persisted})
	if err != nil {
		return err
	}
	normalized := normalizeFindingsJSON(string(raw), "ci")
	if err := e.db.AdmitLateCIFindings(runID, monitor.stepID, head, normalized); err != nil {
		return err
	}
	monitor.findings = normalized
	monitor.response = &approvalResponse{action: action, findingIDs: findingIDList(normalized)}
	if monitor.invalidate != nil {
		monitor.invalidate()
	}
	monitor.cancel()
	return nil
}

func (e *Executor) executeInterruptibleCI(step Step, sctx *StepContext, sr *db.StepResult) (*StepOutcome, error) {
	if step.Name() != types.StepCI || sctx.Fixing {
		return step.Execute(sctx)
	}
	child, cancel := context.WithCancel(sctx.Ctx)
	defer cancel()
	monitor := &lateCIMonitor{runID: sctx.Run.ID, stepID: sr.ID, cancel: cancel, done: make(chan error, 1)}
	if verifier, ok := step.(interface{ VerifyLateCIAdmission(*StepContext) error }); ok {
		monitor.verify = func(ctx context.Context) error {
			copyCtx := *sctx
			copyCtx.Ctx = ctx
			run, err := e.db.GetRun(monitor.runID)
			if err != nil || run == nil {
				return fmt.Errorf("read late CI admission run: %v", err)
			}
			copyCtx.Run = run
			return verifier.VerifyLateCIAdmission(&copyCtx)
		}
	}
	monitor.invalidate = func() {
		if sctx.CIReadinessChanged != nil {
			sctx.CIReadinessChanged(false, false)
		}
	}
	e.mu.Lock()
	e.lateCI = monitor
	e.mu.Unlock()
	copyCtx := *sctx
	copyCtx.Ctx = child
	copyCtx.CIReadinessChanged = func(ready, declared bool) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if monitor.response == nil && sctx.CIReadinessChanged != nil {
			sctx.CIReadinessChanged(ready, declared)
		}
	}
	outcome, err := step.Execute(&copyCtx)
	e.mu.Lock()
	e.lateCI = nil
	response := monitor.response
	if response != nil {
		e.lateCIHandoff = monitor
	}
	e.mu.Unlock()
	if response == nil {
		return outcome, err
	}
	persisted, readErr := e.db.GetRun(sctx.Run.ID)
	if readErr != nil {
		return nil, fmt.Errorf("verify late CI handoff: %w", readErr)
	}
	if persisted == nil {
		return nil, fmt.Errorf("verify late CI handoff: run missing")
	}
	if persisted.PRState != nil && (*persisted.PRState == "merged" || *persisted.PRState == "closed") {
		e.finishLateCIHandoff(fmt.Errorf("late finding retained; PR became terminal before repair"))
		return &StepOutcome{SkipRemaining: true}, nil
	}
	if persisted.Status != types.RunRunning {
		return nil, fmt.Errorf("late CI handoff no longer owns an active run")
	}
	// Parent Stop/shutdown always wins over a queued amendment.
	if sctx.Ctx.Err() != nil {
		return nil, sctx.Ctx.Err()
	}
	// The monitor has returned: no cancelled poll can later publish readiness.
	// Reuse the normal durable gate and user-fix loop rather than starting a run.
	// The admitted gate already revoked CI readiness and review authority.
	if err := e.db.SetRunCIReady(sctx.Run.ID, false); err != nil {
		return nil, err
	}
	if sctx.CIReadinessChanged != nil {
		sctx.CIReadinessChanged(false, false)
	}
	rounds, readErr := e.db.GetRoundsByStep(sr.ID)
	if readErr != nil || len(rounds) == 0 {
		return nil, fmt.Errorf("late CI admission round unavailable: %v", readErr)
	}
	admittedRound := rounds[len(rounds)-1]
	if admittedRound.FindingsJSON == nil || *admittedRound.FindingsJSON != monitor.findings {
		return nil, fmt.Errorf("late CI admission round changed")
	}
	e.approvalCh <- *response
	return &StepOutcome{NeedsApproval: true, Findings: monitor.findings, admittedRound: admittedRound}, nil
}

// finishLateCIHandoff acknowledges only after the original monitor returned
// and the ordinary user-fix loop took custody. Failed handoffs retain their
// durable finding for recovery instead of claiming the repair started.
func (e *Executor) finishLateCIHandoff(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lateCIHandoff != nil {
		e.lateCIHandoff.done <- err
		e.lateCIHandoff = nil
	}
}

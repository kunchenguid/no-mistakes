package tui

import (
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// applyEvent applies one stream frame and reports whether the model must read
// authoritative state.
//
// A stream gap means the daemon coalesced at least one state transition away
// under buffer pressure; the only correct response is one authoritative read,
// because the lost payload is unknowable from the stream.
//
// State frames are guarded by the monotonic revision: a delta applies only
// when it is newer than what the model already holds. That is what makes a
// delta queued before an authoritative snapshot an idempotent no-op after it,
// so draining a backlog behind a gap cannot move state backwards. Frames with
// no revision (an older daemon) always apply, so a new client against an old
// daemon behaves exactly as it does today.
func (m *Model) applyEvent(event ipc.Event) bool {
	if event.Type == ipc.EventStreamGap {
		m.err = nil
		if step := awaitingStep(m.steps); step != nil && step.Status == types.StepStatusFixReview {
			delete(m.stepDiffs, step.StepName)
			delete(m.stepDiffTruncated, step.StepName)
			delete(m.stepDiffLoaded, step.StepName)
		}
		return true
	}
	if ipc.ClassOf(event.Type) == ipc.ClassState && event.StateRev != 0 {
		if event.StateRev <= m.stateRev {
			return false
		}
		m.stateRev = event.StateRev
	}
	switch event.Type {
	case ipc.EventRunUpdated, ipc.EventRunCreated:
		m.err = nil
		if event.Status != nil {
			m.run.Status = types.RunStatus(*event.Status)
		}
		if event.PRURL != nil {
			m.run.PRURL = event.PRURL
		}

	case ipc.EventCIReadinessChanged:
		m.err = nil
		if event.CIReady != nil {
			m.run.CIReady = *event.CIReady
		}
		if event.CIReadyNoCI != nil {
			m.run.CIReadyNoCI = *event.CIReadyNoCI
		}

	case ipc.EventRunCompleted:
		m.err = nil
		if event.Status != nil {
			m.run.Status = types.RunStatus(*event.Status)
		}
		if event.Error != nil {
			m.run.Error = event.Error
		}
		if event.PRURL != nil {
			m.run.PRURL = event.PRURL
		}
		// Carry the CI approval override onto the model so renderOutcomeBanner
		// shows "passed with override" on the live event path, not just after a
		// snapshot. Without this the banner disagrees with axi's outcome word.
		if event.CIOverrideReason != nil {
			m.run.CIOverrideReason = *event.CIOverrideReason
		}
		if m.syntheticSteps {
			m.steps = nil
			m.run.Steps = nil
		}
		m.flushPartialLog()
		m.done = true
		m.invalidateInactiveStepDiffs()

	case ipc.EventStepStarted:
		m.err = nil
		m.syntheticSteps = false
		if event.StepName != nil {
			m.updateStepStatus(*event.StepName, types.StepStatusRunning)
			m.stepStartTimes[*event.StepName] = time.Now()
		}

	case ipc.EventStepCompleted:
		m.err = nil
		m.syntheticSteps = false
		m.flushPartialLog()
		if event.StepName != nil && event.Status != nil {
			m.updateStepStatus(*event.StepName, types.StepStatus(*event.Status))
		}
		if event.StepName != nil && event.Error != nil {
			m.setStepError(*event.StepName, event.Error)
		}
		if event.StepName != nil && event.FixedFindings != nil {
			m.setStepFixedFindings(*event.StepName, *event.FixedFindings)
		}
		if event.StepName != nil && event.ReportedFindings != nil {
			m.setStepReportedFindings(*event.StepName, *event.ReportedFindings)
		}
		// Persist duration so the step continues to display its elapsed time.
		// Prefer the event's execution-only duration; fall back to local timing.
		// For "fixing" status, clear the persisted duration and back-date the
		// start time by the accumulated execution so the live timer continues
		// from where it left off rather than resetting to zero.
		if event.StepName != nil && event.Status != nil && types.StepStatus(*event.Status) == types.StepStatusFixing {
			var accumulated time.Duration
			for _, s := range m.steps {
				if s.StepName == *event.StepName {
					if s.DurationMS != nil {
						accumulated = time.Duration(*s.DurationMS) * time.Millisecond
					} else if startTime, ok := m.stepStartTimes[*event.StepName]; ok {
						accumulated = time.Since(startTime)
					}
					break
				}
			}
			m.setStepDuration(*event.StepName, nil)
			m.stepStartTimes[*event.StepName] = time.Now().Add(-accumulated)
		} else if event.StepName != nil {
			if event.DurationMS != nil {
				m.setStepDuration(*event.StepName, event.DurationMS)
			} else if startTime, ok := m.stepStartTimes[*event.StepName]; ok {
				elapsed := int64(time.Since(startTime).Milliseconds())
				m.setStepDuration(*event.StepName, &elapsed)
			}
		}
		if event.StepName != nil && event.Findings != nil && *event.Findings != "" {
			m.stepFindings[*event.StepName] = *event.Findings
			// Reset diff view when new findings arrive to prevent stale showDiff
			// from a previous step hiding these findings.
			m.showDiff = false
			m.diffOffset = 0
			if event.Status != nil && (types.StepStatus(*event.Status) == types.StepStatusAwaitingApproval || types.StepStatus(*event.Status) == types.StepStatusFixReview) {
				delete(m.findingInstructions, *event.StepName)
				delete(m.addedFindings, *event.StepName)
				m.resetFindingSelection(*event.StepName)
			}
		}
		// The fix-review diff no longer rides the event stream: it is
		// unbounded and one oversized frame would kill the subscription.
		// Entering the gate invalidates any cached diff and requests a fresh
		// one from the run's worktree.
		if event.StepName != nil && event.Status != nil && types.StepStatus(*event.Status) == types.StepStatusFixReview {
			m.showDiff = false
			m.diffOffset = 0
			m.requestStepDiff(*event.StepName, true)
		}

	case ipc.EventLogChunk:
		if event.Content != nil && *event.Content != "" {
			if m.logPartial != "" && len(m.logs) > 0 && m.logs[len(m.logs)-1] == m.logPartial {
				m.logs = m.logs[:len(m.logs)-1]
			}

			text := m.logPartial + *event.Content
			m.logPartial = ""

			if !strings.HasSuffix(text, "\n") {
				idx := strings.LastIndex(text, "\n")
				if idx == -1 {
					m.logPartial = text
					text = ""
				} else {
					m.logPartial = text[idx+1:]
					text = text[:idx+1]
				}
			}

			if text != "" {
				lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
				m.logs = append(m.logs, lines...)
			}
			if m.logPartial != "" {
				m.logs = append(m.logs, m.logPartial)
			}
			if len(m.logs) > 100 {
				m.logs = m.logs[len(m.logs)-100:]
			}
		}
	default:
		return ipc.ClassOf(event.Type) == ipc.ClassState
	}
	return false
}

// applySnapshot replaces model state with an authoritative get_run snapshot.
//
// A snapshot older than what the model already holds is ignored, so two
// reconciliations racing cannot move state backwards either. Findings come
// from the snapshot's persisted step rows, which is why a coalesced
// step_completed carrying findings is recoverable.
func (m *Model) applySnapshot(run *ipc.RunInfo) bool {
	if run == nil || run.StateRev < m.stateRev {
		return false
	}
	m.err = nil
	m.stateRev = run.StateRev
	steps := normalizePipelineSteps(run.ID, run.Status, run.Steps)
	findings := make(map[types.StepName]string, len(steps))
	for _, step := range steps {
		if step.FindingsJSON != nil && *step.FindingsJSON != "" {
			findings[step.StepName] = *step.FindingsJSON
		}
	}
	oldFindings := m.stepFindings
	m.stepFindings = findings
	m.reconcileGateState(steps, oldFindings, findings)
	run.Steps = steps
	m.run = run
	m.steps = steps
	m.syntheticSteps = false
	m.invalidateInactiveStepDiffs()
	for _, s := range steps {
		// A gate reached while the model was out of sync still needs its
		// diff, which is derived rather than carried by the snapshot.
		if s.Status == types.StepStatusFixReview {
			m.showDiff = false
			m.diffOffset = 0
			m.requestStepDiff(s.StepName, true)
		}
	}
	if run.Status == types.RunCompleted || run.Status == types.RunFailed || run.Status == types.RunCancelled {
		m.flushPartialLog()
		m.done = true
	}
	return true
}

func (m *Model) reconcileGateState(steps []ipc.StepResultInfo, oldFindings, findings map[types.StepName]string) {
	oldGate := awaitingStep(m.steps)
	newGate := awaitingStep(steps)
	if sameGateState(oldGate, newGate, oldFindings, findings) {
		return
	}
	if oldGate != nil {
		m.clearGateState(oldGate.StepName)
	}
	if newGate != nil && (oldGate == nil || oldGate.StepName != newGate.StepName) {
		m.clearGateState(newGate.StepName)
	}
	if newGate != nil {
		m.resetFindingSelection(newGate.StepName)
	}
}

func sameGateState(oldGate, newGate *ipc.StepResultInfo, oldFindings, newFindings map[types.StepName]string) bool {
	if oldGate == nil || newGate == nil {
		return oldGate == nil && newGate == nil
	}
	return oldGate.ID == newGate.ID &&
		oldGate.StepName == newGate.StepName &&
		oldGate.Status == newGate.Status &&
		oldGate.RoundCount == newGate.RoundCount &&
		oldGate.FixRoundCount == newGate.FixRoundCount &&
		oldGate.PendingFixSource == newGate.PendingFixSource &&
		oldFindings[oldGate.StepName] == newFindings[newGate.StepName]
}

func (m *Model) clearGateState(step types.StepName) {
	delete(m.findingSelections, step)
	delete(m.findingCursor, step)
	delete(m.findingInstructions, step)
	delete(m.addedFindings, step)
	if m.editor != nil && m.editor.step == step {
		m.editor = nil
	}
}

// requestStepDiff queues one on-demand read of a fix-review gate's
// working-tree diff. At most one request per step is in flight.
func (m *Model) requestStepDiff(step types.StepName, replace bool) {
	if m.stepDiffFetching[step] && !replace {
		return
	}
	if m.reviewRetryDiff {
		m.reviewRetryDiff = false
		m.reviewDiffErr = nil
		m.err = m.reviewRetryError()
	}
	delete(m.stepDiffs, step)
	delete(m.stepDiffTruncated, step)
	delete(m.stepDiffLoaded, step)
	m.stepDiffRequestID[step]++
	m.stepDiffFetching[step] = true
	m.pendingDiffFetch = append(m.pendingDiffFetch, stepDiffRequest{
		step:      step,
		requestID: m.stepDiffRequestID[step],
	})
}

func (m Model) approvalReady(step *ipc.StepResultInfo) bool {
	if m.reconcilePending || m.reviewRetryReconcile {
		return false
	}
	if step == nil || step.Status != types.StepStatusFixReview {
		return true
	}
	return !m.reviewRetryDiff && m.stepDiffLoaded[step.StepName] && !m.stepDiffFetching[step.StepName]
}

func (m Model) reviewRetryAvailable() bool {
	return m.reviewRetryReconcile || (m.reviewRetryDiff && m.hasFixReviewStep())
}

func (m Model) reviewRetryError() error {
	if m.reviewReconcileErr != nil {
		return m.reviewReconcileErr
	}
	if !m.hasFixReviewStep() {
		return nil
	}
	return m.reviewDiffErr
}

func (m *Model) updateStepStatus(name types.StepName, status types.StepStatus) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].Status = status
			if status != types.StepStatusFixReview {
				m.invalidateStepDiff(name)
			}
			return
		}
	}
}

func (m Model) stepInFixReview(name types.StepName) bool {
	if m.done || m.run == nil || m.run.Status == types.RunCompleted || m.run.Status == types.RunFailed || m.run.Status == types.RunCancelled {
		return false
	}
	for i := range m.steps {
		if m.steps[i].StepName == name {
			return m.steps[i].Status == types.StepStatusFixReview
		}
	}
	return false
}

func (m Model) hasFixReviewStep() bool {
	for i := range m.steps {
		if m.stepInFixReview(m.steps[i].StepName) {
			return true
		}
	}
	return false
}

func (m *Model) invalidateInactiveStepDiffs() {
	for step := range m.stepDiffRequestID {
		if !m.stepInFixReview(step) {
			m.invalidateStepDiff(step)
		}
	}
}

func (m *Model) invalidateStepDiff(step types.StepName) {
	m.stepDiffRequestID[step]++
	delete(m.stepDiffFetching, step)
	delete(m.stepDiffLoaded, step)
	delete(m.stepDiffs, step)
	delete(m.stepDiffTruncated, step)
	if len(m.pendingDiffFetch) > 0 {
		pending := m.pendingDiffFetch[:0]
		for _, request := range m.pendingDiffFetch {
			if request.step != step {
				pending = append(pending, request)
			}
		}
		m.pendingDiffFetch = pending
	}
	if m.reviewRetryDiff && !m.hasFixReviewStep() {
		m.reviewRetryDiff = false
		m.reviewDiffErr = nil
		m.err = m.reviewRetryError()
	}
}

func (m *Model) flushPartialLog() {
	if m.logPartial == "" {
		return
	}
	if len(m.logs) > 0 && m.logs[len(m.logs)-1] == m.logPartial {
		m.logPartial = ""
		return
	}
	m.logs = append(m.logs, m.logPartial)
	m.logPartial = ""
	if len(m.logs) > 100 {
		m.logs = m.logs[len(m.logs)-100:]
	}
}

func (m *Model) setStepDuration(name types.StepName, durationMS *int64) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].DurationMS = durationMS
			return
		}
	}
}

func (m *Model) setStepError(name types.StepName, errMsg *string) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].Error = errMsg
			return
		}
	}
}

func (m *Model) setStepFixedFindings(name types.StepName, fixedFindings int) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].FixedFindings = fixedFindings
			return
		}
	}
}

func (m *Model) setStepReportedFindings(name types.StepName, reportedFindings int) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].ReportedFindings = reportedFindings
			return
		}
	}
}

// stepsWithRunningElapsed returns a copy of m.steps with DurationMS set on
// running/fixing steps based on their recorded start times.
func (m Model) stepsWithRunningElapsed() []ipc.StepResultInfo {
	steps := make([]ipc.StepResultInfo, len(m.steps))
	copy(steps, m.steps)
	for i := range steps {
		if steps[i].DurationMS != nil {
			continue
		}
		switch steps[i].Status {
		case types.StepStatusRunning, types.StepStatusFixing,
			types.StepStatusAwaitingApproval, types.StepStatusFixReview:
			if startTime, ok := m.stepStartTimes[steps[i].StepName]; ok {
				elapsed := int64(time.Since(startTime).Milliseconds())
				steps[i].DurationMS = &elapsed
			}
		}
	}
	return steps
}

package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const spinnerTickInterval = 120 * time.Millisecond

// reconcileTimeout bounds one authoritative run read so a wedged daemon
// surfaces as an error instead of a silently stalled view.
const reconcileTimeout = 30 * time.Second

// Reconnect pacing for a dropped event stream. Bounded so a daemon that is
// genuinely gone surfaces the error instead of spinning on failed dials.
const (
	resubscribeDelay    = 500 * time.Millisecond
	maxResubscribeTries = 60
)

var runBrowserCommand = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// openBrowserCmd returns a tea.Cmd that opens the given URL in the default browser.
func openBrowserCmd(url string) tea.Cmd {
	return func() tea.Msg {
		name, args := browserCommandSpec(runtime.GOOS, url)
		if err := runBrowserCommand(name, args...); err != nil {
			return errMsg{fmt.Errorf("open PR: %w", err)}
		}
		return nil
	}
}

func browserCommandSpec(goos, url string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		return "xdg-open", []string{url}
	}
}

func canRerun(run *ipc.RunInfo) bool {
	if run == nil {
		return false
	}
	switch run.Status {
	case types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}

func (m Model) rerunCmd(requestID uint64) tea.Cmd {
	if !canRerun(m.run) || m.client == nil || m.run == nil {
		return nil
	}
	repoID := m.run.RepoID
	branch := m.run.Branch
	previousRunID := m.run.ID
	return func() tea.Msg {
		var rerun ipc.RerunResult
		if err := m.client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repoID, Branch: branch, PreviousRunID: previousRunID}, &rerun); err != nil {
			return rerunErrMsg{err: err, requestID: requestID}
		}
		var result ipc.GetRunResult
		if err := m.client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: rerun.RunID}, &result); err != nil {
			return rerunErrMsg{err: fmt.Errorf("load rerun: %w", err), requestID: requestID}
		}
		if result.Run == nil {
			return rerunErrMsg{err: fmt.Errorf("load rerun: run %s not found", rerun.RunID), requestID: requestID}
		}
		return rerunStartedMsg{run: result.Run, requestID: requestID}
	}
}

// maybeAutoApproveCmd auto-resolves the current awaiting step when yolo mode is
// on, returning nil otherwise. Yolo means "agree to fix every finding": a gate
// whose findings are actionable gets a fix request (all findings selected),
// while a gate with only non-actionable (no-op) findings - or none at all - is
// approved as-is. A step is fixed at most once; the fix re-runs the step and
// re-enters the gate as a fix_review, which yolo then approves so the pipeline
// runs to completion without looping. Each terminal action fires once so
// duplicate events while waiting for the round-trip don't resend it.
func (m Model) maybeAutoApproveCmd() tea.Cmd {
	if !m.yoloMode {
		return nil
	}
	step := awaitingStep(m.steps)
	if step == nil || m.yoloApproved[step.StepName] {
		return nil
	}
	if !m.approvalReady(step) {
		return nil
	}
	if step.Status != types.StepStatusFixReview && !m.yoloFixed[step.StepName] && m.stepHasActionableFindings(step.StepName) {
		m.yoloFixed[step.StepName] = true
		m.resetFindingSelection(step.StepName)
		return m.respondCmd(types.ActionFix)
	}
	m.yoloApproved[step.StepName] = true
	return m.respondCmd(types.ActionApprove)
}

func (m Model) respondCmd(action types.ApprovalAction) tea.Cmd {
	step := awaitingStep(m.steps)
	if step == nil {
		return nil
	}
	if !m.approvalReady(step) {
		return nil
	}
	if action == types.ActionFix {
		ids := m.selectedFindingIDs(step.StepName)
		userAdded := m.selectedUserAddedFindings(step.StepName)
		if len(ids) == 0 && len(userAdded) == 0 && len(m.findingItems(step.StepName)) > 0 {
			return nil
		}
	}
	return func() tea.Msg {
		params := &ipc.RespondParams{
			RunID:  m.runID,
			Step:   step.StepName,
			Action: action,
		}
		if action == types.ActionFix {
			ids := m.selectedFindingIDs(step.StepName)
			if len(ids) > 0 {
				params.FindingIDs = ids
				if byStep := m.findingInstructions[step.StepName]; len(byStep) > 0 {
					filtered := make(map[string]string, len(byStep))
					for _, id := range ids {
						if note, ok := byStep[id]; ok && note != "" {
							filtered[id] = note
						}
					}
					if len(filtered) > 0 {
						params.Instructions = filtered
					}
				}
			}
			if added := m.selectedUserAddedFindings(step.StepName); len(added) > 0 {
				params.AddedFindings = append([]types.Finding(nil), added...)
			}
		}
		var result ipc.RespondResult
		err := m.client.Call(ipc.MethodRespond, params, &result)
		if err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func (m *Model) retryReviewCmd() tea.Cmd {
	var cmds []tea.Cmd
	if m.reviewRetryReconcile {
		m.reviewRetryReconcile = false
		m.reviewReconcileErr = nil
		cmds = append(cmds, m.reconcileCmd())
	}
	if m.reviewRetryDiff {
		step := awaitingStep(m.steps)
		if step != nil && step.Status == types.StepStatusFixReview {
			m.reviewRetryDiff = false
			m.reviewDiffErr = nil
			m.requestStepDiff(step.StepName, true)
			cmds = append(cmds, m.drainDiffFetches()...)
		}
	}
	m.err = m.reviewRetryError()
	return tea.Batch(cmds...)
}

func (m Model) cancelRunCmd() tea.Cmd {
	if m.runID == "" {
		return nil
	}
	return func() tea.Msg {
		params := &ipc.CancelRunParams{RunID: m.runID}
		var result ipc.CancelRunResult
		err := m.client.Call(ipc.MethodCancelRun, params, &result)
		if err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func (m Model) subscribeCmd() tea.Cmd {
	return func() tea.Msg {
		events, cancel, err := ipc.Subscribe(m.socketPath, &ipc.SubscribeParams{
			RunID: m.runID,
		})
		if err != nil {
			return subscriptionErrMsg{err: fmt.Errorf("subscribe: %w", err), subscriptionID: m.subscriptionID}
		}
		return connectedMsg{events: events, cancelSub: cancel, subscriptionID: m.subscriptionID}
	}
}

// reconcileCmd reads authoritative run state exactly once.
//
// Requests coalesce: at most one read is in flight, and a gap arriving while
// one is running schedules exactly one follow-up. That keeps overflow from
// turning into a reconciliation storm, and it is the only read the TUI ever
// makes on its own - there is no interval poll.
func (m *Model) reconcileCmd() tea.Cmd {
	if m.reconcilePending {
		m.reconcileAgain = true
		return nil
	}
	if m.reviewRetryReconcile {
		m.reviewRetryReconcile = false
		m.reviewReconcileErr = nil
		m.err = m.reviewRetryError()
	}
	m.reconcilePending = true
	subID := m.subscriptionID
	reconcile := m.reconcile
	client := m.client
	runID := m.runID
	return func() tea.Msg {
		if reconcile != nil {
			ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
			defer cancel()
			run, err := reconcile(ctx)
			return runReconciledMsg{run: run, err: err, subscriptionID: subID}
		}
		var result ipc.GetRunResult
		if err := client.CallWithTimeout(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result, reconcileTimeout); err != nil {
			return runReconciledMsg{err: err, subscriptionID: subID}
		}
		return runReconciledMsg{run: result.Run, subscriptionID: subID}
	}
}

// drainDiffFetches converts queued fix-review diff requests into commands.
func (m *Model) drainDiffFetches() []tea.Cmd {
	if len(m.pendingDiffFetch) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(m.pendingDiffFetch))
	for _, request := range m.pendingDiffFetch {
		cmds = append(cmds, m.fetchStepDiffCmd(request))
	}
	m.pendingDiffFetch = nil
	return cmds
}

// fetchStepDiffCmd reads one fix-review gate's working-tree diff on demand.
// The diff is derived from the run's worktree rather than carried by the event
// stream, where an unbounded frame would break the subscription.
func (m *Model) fetchStepDiffCmd(request stepDiffRequest) tea.Cmd {
	subID := m.subscriptionID
	fetch := m.fetchStepDiff
	client := m.client
	runID := m.runID
	return func() tea.Msg {
		if fetch != nil {
			diff, err := fetch(request.step)
			return stepDiffMsg{step: request.step, diff: diff, err: err, requestID: request.requestID, subscriptionID: subID}
		}
		var result ipc.GetStepDiffResult
		if err := client.CallWithTimeout(ipc.MethodGetStepDiff, &ipc.GetStepDiffParams{RunID: runID}, &result, reconcileTimeout); err != nil {
			return stepDiffMsg{step: request.step, err: err, requestID: request.requestID, subscriptionID: subID}
		}
		return stepDiffMsg{step: request.step, diff: result.Diff, truncated: result.Truncated, requestID: request.requestID, subscriptionID: subID}
	}
}

// resubscribeCmd waits out the reconnect delay before reattaching to the
// event stream.
func (m Model) resubscribeCmd() tea.Cmd {
	subID := m.subscriptionID
	return tea.Tick(resubscribeDelay, func(time.Time) tea.Msg {
		return resubscribeMsg{subscriptionID: subID}
	})
}

func (m *Model) startResubscribe() tea.Cmd {
	if m.resubscribeTries >= maxResubscribeTries {
		return nil
	}
	m.resubscribeTries++
	m.streamClosed = false
	m.subscriptionID++
	m.stateRev = 0
	m.reconcilePending = false
	m.reconcileAgain = false
	m.stepDiffFetching = make(map[types.StepName]bool)
	m.stepDiffLoaded = make(map[types.StepName]bool)
	m.stepDiffs = make(map[types.StepName]string)
	m.stepDiffTruncated = make(map[types.StepName]bool)
	m.pendingDiffFetch = nil
	return m.resubscribeCmd()
}

func (m Model) waitForEvent() tea.Cmd {
	events := m.events
	if events == nil {
		return nil
	}
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return subscriptionErrMsg{err: fmt.Errorf("event stream closed"), subscriptionID: m.subscriptionID}
		}
		return eventMsg{event: event, subscriptionID: m.subscriptionID}
	}
}

func (m Model) refreshSyncCmd() tea.Cmd {
	refresh := m.syncRefresh
	if refresh == nil {
		return nil
	}
	return func() tea.Msg {
		started := time.Now()
		state := refresh()
		result := "refused"
		if branchsync.CanApply(state) || state.State == branchsync.StateSynchronized || state.State == branchsync.StateMergedRemoteRemoved {
			result = "noop"
		}
		trackTUISyncAttempt("check", state, result, started)
		return syncRefreshedMsg{state: state}
	}
}

func (m Model) applySyncCmd() tea.Cmd {
	apply := m.syncApply
	if apply == nil {
		return nil
	}
	return func() tea.Msg {
		started := time.Now()
		state := apply()
		result := "refused"
		if state.Changed && state.State == branchsync.StateSynchronized {
			result = "applied"
		} else if state.State == branchsync.StateSynchronized || state.State == branchsync.StateMergedRemoteRemoved {
			result = "noop"
		}
		trackTUISyncAttempt("apply", state, result, started)
		return syncAppliedMsg{state: state}
	}
}

func (m Model) applyRecoverCmd() tea.Cmd {
	recover := m.syncRecover
	if recover == nil {
		return nil
	}
	keepLocal := m.branchSync != nil && m.branchSync.Recovery != nil && m.branchSync.Recovery.KeepLocal
	return func() tea.Msg {
		started := time.Now()
		state := recover(keepLocal)
		result := "refused"
		if state.Recovered && state.Changed {
			result = "applied"
		} else if state.Recovered {
			result = "noop"
		}
		mode := "recover"
		if keepLocal {
			mode = "recover_keep_local"
		}
		trackTUISyncAttempt(mode, state, result, started)
		return syncAppliedMsg{state: state}
	}
}

func (m Model) spinnerTickCmd() tea.Cmd {
	return tea.Tick(spinnerTickInterval, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

func (m Model) hasSpinningStep() bool {
	for _, step := range m.steps {
		switch step.Status {
		case types.StepStatusRunning, types.StepStatusFixing:
			return true
		}
	}
	return false
}

func (m *Model) startSpinnerIfNeeded() tea.Cmd {
	if m.done || m.quitting || m.spinnerScheduled || !m.hasSpinningStep() {
		return nil
	}
	m.spinnerScheduled = true
	return m.spinnerTickCmd()
}

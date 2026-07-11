package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const spinnerTickInterval = 120 * time.Millisecond

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
	return func() tea.Msg {
		var rerun ipc.RerunResult
		if err := m.client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repoID, Branch: branch}, &rerun); err != nil {
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
// on, returning nil otherwise. Yolo means "agree to fix every selectable
// finding": a gate whose findings are actionable gets a fix request only when
// at least one finding has an ID, while a gate with only non-actionable (no-op)
// findings, no selectable findings, or no findings at all is approved as-is.
// A step is fixed at most once; the fix re-runs the step and re-enters the gate
// as a fix_review, which yolo then approves so the pipeline
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
	if step.Status != types.StepStatusFixReview && !m.yoloFixed[step.StepName] && m.stepHasActionableFindings(step.StepName) {
		m.resetFindingSelection(step.StepName)
		if ids := m.selectedFindingIDs(step.StepName); len(ids) > 0 {
			m.yoloFixed[step.StepName] = true
			return m.yoloResolveCmd(step.StepName, true, ids)
		}
	}
	m.yoloApproved[step.StepName] = true
	return m.yoloResolveCmd(step.StepName, false, nil)
}

// yoloResolveCmd resolves a gate under unattended (yolo) consent, failing
// closed: it re-fetches the run and aborts instead of fixing again or approving
// when a blocking finding lineage remains unresolved after its repair cascade,
// rather than accepting merely because a fix was attempted.
func (m Model) yoloResolveCmd(stepName types.StepName, fix bool, fixIDs []string) tea.Cmd {
	client := m.client
	runID := m.runID
	return func() tea.Msg {
		var got ipc.GetRunResult
		if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &got); err != nil {
			return errMsg{fmt.Errorf("refresh run before yolo response: %w", err)}
		}
		if got.Run == nil {
			return errMsg{fmt.Errorf("refresh run before yolo response: run %s not found", runID)}
		}
		if got.Run.BlockingRepairUnresolved {
			var result ipc.RespondResult
			if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{
				RunID:  runID,
				Step:   stepName,
				Action: types.ActionAbort,
			}, &result); err != nil {
				return errMsg{fmt.Errorf("abort yolo response with unresolved blocking repair: %w", err)}
			}
			if !result.OK {
				return errMsg{fmt.Errorf("abort yolo response with unresolved blocking repair: daemon rejected the response")}
			}
			return nil
		}
		params := &ipc.RespondParams{RunID: runID, Step: stepName, Action: types.ActionApprove}
		if fix {
			params.Action = types.ActionFix
			params.FindingIDs = fixIDs
		}
		var result ipc.RespondResult
		if err := client.Call(ipc.MethodRespond, params, &result); err != nil {
			return errMsg{err}
		}
		if !result.OK {
			return errMsg{fmt.Errorf("yolo response rejected by daemon")}
		}
		return nil
	}
}

func (m Model) respondCmd(action types.ApprovalAction) tea.Cmd {
	step := awaitingStep(m.steps)
	if step == nil {
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
		var result ipc.GetRunResult
		if err := m.client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: m.runID}, &result); err != nil {
			cancel()
			return subscriptionErrMsg{err: fmt.Errorf("refresh subscribed run: %w", err), subscriptionID: m.subscriptionID}
		}
		if result.Run == nil {
			cancel()
			return subscriptionErrMsg{err: fmt.Errorf("refresh subscribed run: run %s not found", m.runID), subscriptionID: m.subscriptionID}
		}
		return connectedMsg{events: events, run: result.Run, cancelSub: cancel, subscriptionID: m.subscriptionID}
	}
}
func (m Model) refreshRunCmd() tea.Cmd {
	return func() tea.Msg {
		var result ipc.GetRunResult
		if err := m.client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: m.runID}, &result); err != nil {
			return subscriptionErrMsg{err: fmt.Errorf("refresh run routing: %w", err), subscriptionID: m.subscriptionID}
		}
		if result.Run == nil {
			return subscriptionErrMsg{err: fmt.Errorf("refresh run routing: run %s not found", m.runID), subscriptionID: m.subscriptionID}
		}
		return runRefreshedMsg{run: result.Run, subscriptionID: m.subscriptionID}
	}
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

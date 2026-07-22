package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Model is the root bubbletea model for the TUI.
type Model struct {
	// Connection.
	socketPath     string
	client         *ipc.Client
	events         <-chan ipc.Event
	cancelSub      func()
	runID          string
	subscriptionID uint64

	// State.
	run *ipc.RunInfo
	// stateRev is the highest run-state revision this model has applied. It
	// is scoped to the current subscription generation: a fresh subscription
	// resets it to zero and reconciles, adopting the daemon's numbering.
	stateRev int64
	// reconcile reads authoritative run state. Nil in production, where the
	// IPC client is used; injectable so the contract is testable without a
	// daemon.
	reconcile            func(ctx context.Context) (*ipc.RunInfo, error)
	reconcilePending     bool
	reconcileAgain       bool
	streamClosed         bool
	reviewRetryReconcile bool
	reviewRetryDiff      bool
	reviewReconcileErr   error
	reviewDiffErr        error
	// resubscribeTries bounds reconnect attempts for a dropped event stream.
	resubscribeTries int
	// fetchStepDiff reads a fix-review gate's working-tree diff. Nil in
	// production, where the IPC client is used; injectable for tests.
	fetchStepDiff       func(types.StepName) (string, error)
	steps               []ipc.StepResultInfo
	stepFindings        map[types.StepName]string            // step name → raw findings JSON
	stepDiffs           map[types.StepName]string            // step name → raw unified diff (fetched on demand)
	stepDiffTruncated   map[types.StepName]bool              // steps whose fetched diff was capped
	stepDiffFetching    map[types.StepName]bool              // steps with an in-flight diff read
	stepDiffLoaded      map[types.StepName]bool              // steps whose current diff request completed
	stepDiffRequestID   map[types.StepName]uint64            // latest request generation per step
	pendingDiffFetch    []stepDiffRequest                    // diff reads to issue on the next update
	findingSelections   map[types.StepName]map[string]bool   // step name → finding ID → selected
	findingCursor       map[types.StepName]int               // step name → current finding cursor
	findingInstructions map[types.StepName]map[string]string // step name → finding ID → user note
	addedFindings       map[types.StepName][]types.Finding   // user-authored findings per step
	editor              *editorState                         // active modal editor (nil when none)
	logs                []string
	logPartial          string // buffered partial line (no trailing newline yet)

	// Timing.
	stepStartTimes map[types.StepName]time.Time // when each step started running

	// UI.
	width            int
	height           int
	latestVersion    string
	err              error
	quitting         bool
	done             bool // run completed or failed
	rerunPending     bool
	rerunRequestID   uint64
	showDiff         bool // toggle diff viewer
	showHelp         bool // toggle help overlay
	confirmAbort     bool // true after first x press, next x actually aborts
	diffOffset       int  // scroll position in diff view
	spinnerFrame     int
	spinnerScheduled bool
	syntheticSteps   bool
	yoloMode         bool                    // auto-resolve every step awaiting human action
	yoloApproved     map[types.StepName]bool // steps already finalized (approved) this run
	yoloFixed        map[types.StepName]bool // steps yolo has already requested a fix for

	// Guarded local-branch synchronization. Cached state is rendered passively;
	// only the explicit u flow calls Refresh or Apply.
	branchSync     *branchsync.State
	syncService    *branchsync.Service
	syncRefresh    func() branchsync.State
	syncApply      func() branchsync.State
	syncRecover    func() branchsync.State
	syncConfirm    bool
	recoverConfirm bool
	syncRefreshing bool
}

// NewModel creates a TUI model for the given run.
// The client should already be connected to the daemon.
func NewModel(socketPath string, client *ipc.Client, run *ipc.RunInfo) Model {
	syntheticSteps := len(run.Steps) == 0 && shouldBackfillPipelineSteps(run.Status, run.Steps, types.AllSteps())
	steps := normalizePipelineSteps(run.ID, run.Status, run.Steps)
	run.Steps = steps
	m := Model{
		socketPath:          socketPath,
		client:              client,
		runID:               run.ID,
		subscriptionID:      1,
		run:                 run,
		done:                run.Status.Terminal(),
		steps:               steps,
		stepFindings:        make(map[types.StepName]string),
		stepDiffs:           make(map[types.StepName]string),
		stepDiffTruncated:   make(map[types.StepName]bool),
		stepDiffFetching:    make(map[types.StepName]bool),
		stepDiffLoaded:      make(map[types.StepName]bool),
		stepDiffRequestID:   make(map[types.StepName]uint64),
		findingSelections:   make(map[types.StepName]map[string]bool),
		findingCursor:       make(map[types.StepName]int),
		findingInstructions: make(map[types.StepName]map[string]string),
		addedFindings:       make(map[types.StepName][]types.Finding),
		stepStartTimes:      make(map[types.StepName]time.Time),
		syntheticSteps:      syntheticSteps,
		yoloApproved:        make(map[types.StepName]bool),
		yoloFixed:           make(map[types.StepName]bool),
	}
	// Populate findings and start times from initial step data (for re-attach scenarios).
	for _, s := range steps {
		if s.FindingsJSON != nil && *s.FindingsJSON != "" {
			m.stepFindings[s.StepName] = *s.FindingsJSON
			if s.Status == types.StepStatusAwaitingApproval || s.Status == types.StepStatusFixReview {
				m.resetFindingSelection(s.StepName)
			}
		}
		// Seed start times from DB so elapsed time can be computed on re-attach.
		if s.StartedAt != nil && s.DurationMS == nil {
			m.stepStartTimes[s.StepName] = time.Unix(*s.StartedAt, 0)
		}
	}
	return m
}

func normalizePipelineSteps(runID string, runStatus types.RunStatus, steps []ipc.StepResultInfo) []ipc.StepResultInfo {
	knownSteps := types.AllSteps()
	if !shouldBackfillPipelineSteps(runStatus, steps, knownSteps) {
		return steps
	}

	byName := make(map[types.StepName]ipc.StepResultInfo, len(steps))
	for _, step := range steps {
		byName[step.StepName] = step
	}

	normalized := make([]ipc.StepResultInfo, 0, len(knownSteps)+len(steps))
	for _, stepName := range knownSteps {
		step, ok := byName[stepName]
		if !ok {
			normalized = append(normalized, ipc.StepResultInfo{
				RunID:     runID,
				StepName:  stepName,
				StepOrder: stepName.Order(),
				Status:    types.StepStatusPending,
			})
			continue
		}
		if step.RunID == "" {
			step.RunID = runID
		}
		if step.StepOrder == 0 {
			step.StepOrder = stepName.Order()
		}
		normalized = append(normalized, step)
		delete(byName, stepName)
	}

	for _, step := range steps {
		if _, ok := byName[step.StepName]; !ok {
			continue
		}
		normalized = append(normalized, step)
		delete(byName, step.StepName)
	}

	return normalized
}

func shouldBackfillPipelineSteps(runStatus types.RunStatus, steps []ipc.StepResultInfo, knownSteps []types.StepName) bool {
	if len(steps) == 0 {
		return runStatus == types.RunPending || runStatus == types.RunRunning
	}
	if len(steps) >= len(knownSteps) {
		return false
	}
	for i, step := range steps {
		if step.StepName != knownSteps[i] {
			return false
		}
	}
	return true
}

func (m Model) Init() tea.Cmd {
	return m.subscribeCmd()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case connectedMsg:
		if msg.subscriptionID != m.subscriptionID {
			if msg.cancelSub != nil {
				msg.cancelSub()
			}
			return m, nil
		}
		m.events = msg.events
		m.cancelSub = msg.cancelSub
		m.streamClosed = false
		if m.done {
			if m.cancelSub != nil {
				m.cancelSub()
			}
			return m, nil
		}
		return m, tea.Batch(m.waitForEvent(), m.startSpinnerIfNeeded())

	case rerunStartedMsg:
		if msg.requestID != m.rerunRequestID {
			return m, nil
		}
		m.rerunPending = false
		if m.cancelSub != nil {
			m.cancelSub()
		}
		m.resetForRun(msg.run)
		if m.done {
			return m, nil
		}
		return m, tea.Batch(m.subscribeCmd(), m.startSpinnerIfNeeded())

	case rerunErrMsg:
		if msg.requestID != m.rerunRequestID {
			return m, nil
		}
		m.rerunPending = false
		m.err = msg.err
		return m, nil

	case eventMsg:
		if msg.subscriptionID != m.subscriptionID {
			return m, nil
		}
		gapped := m.applyEvent(msg.event)
		m.refreshCachedSync()
		cmds := m.drainDiffFetches()
		if gapped {
			// The daemon coalesced at least one state transition away.
			// Read authoritative state once instead of guessing.
			cmds = append(cmds, m.reconcileCmd())
		}
		if m.done {
			return m, tea.Batch(cmds...)
		}
		cmds = append(cmds, m.waitForEvent(), m.startSpinnerIfNeeded(), m.maybeAutoApproveCmd())
		return m, tea.Batch(cmds...)

	case runReconciledMsg:
		if msg.subscriptionID != m.subscriptionID {
			return m, nil
		}
		m.reconcilePending = false
		if msg.err != nil {
			m.reconcileAgain = false
			m.reviewRetryReconcile = true
			m.reviewReconcileErr = fmt.Errorf("refresh authoritative run state: %w", msg.err)
			m.err = m.reviewRetryError()
		} else {
			m.reviewRetryReconcile = false
			m.reviewReconcileErr = nil
			if msg.run != nil && !m.applySnapshot(msg.run) {
				m.reconcileAgain = true
			}
			m.err = m.reviewRetryError()
			m.resubscribeTries = 0
		}
		cmds := m.drainDiffFetches()
		if m.reconcileAgain {
			m.reconcileAgain = false
			cmds = append(cmds, m.reconcileCmd())
		} else if m.streamClosed && !m.done {
			cmds = append(cmds, m.startResubscribe())
		}
		return m, tea.Batch(cmds...)

	case subscriptionErrMsg:
		if msg.subscriptionID != m.subscriptionID {
			return m, nil
		}
		m.err = msg.err
		if m.done {
			return m, nil
		}
		if m.cancelSub != nil {
			m.cancelSub()
			m.cancelSub = nil
		}
		m.events = nil
		m.streamClosed = true
		if m.reconcilePending {
			return m, nil
		}
		return m, m.startResubscribe()

	case resubscribeMsg:
		if msg.subscriptionID != m.subscriptionID {
			return m, nil
		}
		return m, m.subscribeCmd()

	case stepDiffMsg:
		if msg.subscriptionID != m.subscriptionID {
			return m, nil
		}
		if msg.requestID != m.stepDiffRequestID[msg.step] {
			return m, nil
		}
		if !m.stepInFixReview(msg.step) {
			m.invalidateStepDiff(msg.step)
			return m, nil
		}
		delete(m.stepDiffFetching, msg.step)
		if msg.err == nil {
			m.reviewRetryDiff = false
			m.reviewDiffErr = nil
			m.err = m.reviewRetryError()
			m.stepDiffLoaded[msg.step] = true
			m.stepDiffTruncated[msg.step] = msg.truncated
			if msg.diff != "" {
				m.stepDiffs[msg.step] = msg.diff
			} else {
				delete(m.stepDiffs, msg.step)
			}
			return m, func() tea.Msg {
				return stepDiffReadyMsg{
					step:           msg.step,
					requestID:      msg.requestID,
					subscriptionID: msg.subscriptionID,
				}
			}
		}
		delete(m.stepDiffLoaded, msg.step)
		delete(m.stepDiffs, msg.step)
		delete(m.stepDiffTruncated, msg.step)
		m.reviewRetryDiff = true
		m.reviewDiffErr = fmt.Errorf("load fix-review diff: %w", msg.err)
		m.err = m.reviewRetryError()
		return m, nil

	case stepDiffReadyMsg:
		if msg.subscriptionID != m.subscriptionID || msg.requestID != m.stepDiffRequestID[msg.step] {
			return m, nil
		}
		return m, m.maybeAutoApproveCmd()

	case syncRefreshedMsg:
		m.syncRefreshing = false
		m.branchSync = &msg.state
		m.syncConfirm = branchsync.CanApply(msg.state)
		if msg.state.Error != "" && !m.syncConfirm {
			m.err = fmt.Errorf("branch sync: %s", msg.state.Error)
		}
		return m, nil

	case syncAppliedMsg:
		m.syncRefreshing = false
		m.syncConfirm = false
		m.recoverConfirm = false
		m.branchSync = &msg.state
		if msg.state.Error != "" {
			m.err = fmt.Errorf("branch sync: %s", msg.state.Error)
		}
		return m, nil

	case spinnerTickMsg:
		m.spinnerScheduled = false
		if !m.hasSpinningStep() {
			return m, nil
		}
		if len(spinnerFrames) > 0 {
			m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		}
		if m.done || m.quitting {
			return m, nil
		}
		return m, m.startSpinnerIfNeeded()

	case errMsg:
		m.err = msg.err
		return m, nil
	}

	return m, nil
}

// terminalTitle returns the current terminal title string based on run state.
// Format: "<symbol> <status> - <branch_name>"
func (m Model) terminalTitle() string {
	branch := ""
	if m.run != nil {
		branch = m.run.Branch
	}
	suffix := ""
	if branch != "" {
		suffix = " - " + branch
	}

	// Terminal states.
	if m.done || m.run == nil {
		switch {
		case m.run == nil:
			return "○ Pending" + suffix
		case m.run.Status == types.RunCompleted:
			return "✓ Completed" + suffix
		case m.run.Status == types.RunFailed:
			return "✗ Failed" + suffix
		case m.run.Status == types.RunCancelled:
			return "✗ Cancelled" + suffix
		case m.run.Status == types.RunCIMonitorInterrupted:
			return "CI monitor interrupted" + suffix
		}
	}

	// Find the most relevant active step.
	for _, s := range m.steps {
		icon := stepStatusIndicator(s.Status, m.spinnerFrame)
		switch s.Status {
		case types.StepStatusRunning, types.StepStatusFixing:
			activity := cimonitor.FromAuthoritative(m.run != nil && m.run.CIReady, m.run != nil && m.run.CIReadyNoCI, m.logs)
			if s.StepName == types.StepCI && activity.Ready {
				return "✓ Checks passed" + suffix
			}
			return icon + " " + stepLabel(s.StepName) + suffix
		case types.StepStatusAwaitingApproval, types.StepStatusFixReview:
			return icon + " " + stepLabel(s.StepName) + suffix
		}
	}

	return "○ Pending" + suffix
}

// setTerminalTitle returns the OSC escape sequence to set the terminal title.
func setTerminalTitle(title string) string {
	return "\033]2;" + title + "\007"
}

func (m *Model) resetForRun(run *ipc.RunInfo) {
	width, height := m.width, m.height
	nextSubscriptionID := m.subscriptionID + 1
	latestVersion := m.latestVersion
	fresh := NewModel(m.socketPath, m.client, run)
	fresh.width = width
	fresh.height = height
	fresh.subscriptionID = nextSubscriptionID
	fresh.rerunRequestID = m.rerunRequestID
	fresh.latestVersion = latestVersion
	*m = fresh
}

// Run starts the TUI program.
func (m *Model) refreshCachedSync() {
	if m.syncService == nil {
		return
	}
	state := m.syncService.InspectCached(context.Background())
	if state.Pipeline.RunID == m.runID && tuiRelevantSyncState(state) {
		m.branchSync = &state
	} else {
		m.branchSync = nil
	}
}

// Run starts the TUI and wires the same guarded synchronization service used
// by the human and AXI commands. Opening the TUI performs cached inspection
// only and never contacts a remote.
func Run(socketPath string, client *ipc.Client, run *ipc.RunInfo, latestVersion string) error {
	model := NewModel(socketPath, client, run)
	model.latestVersion = latestVersion
	if service, closeFn, err := branchsync.OpenCurrent(); err == nil {
		defer closeFn()
		model.syncService = service
		model.syncRefresh = func() branchsync.State { return service.Refresh(context.Background()) }
		model.syncApply = func() branchsync.State { return service.Apply(context.Background()) }
		model.syncRecover = func() branchsync.State { return service.Recover(context.Background(), false) }
		model.refreshCachedSync()
	}
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func tuiRelevantSyncState(state branchsync.State) bool {
	switch state.State {
	case branchsync.StatePipelineOwned, branchsync.StatePushInProgress, branchsync.StateBehind,
		branchsync.StateLocalAhead, branchsync.StateDiverged, branchsync.StateDirty,
		branchsync.StateMergedRemoteRetained, branchsync.StateMergedRemoteRemoved,
		branchsync.StateClosed, branchsync.StateTargetChanged, branchsync.StateCustodyReturned,
		branchsync.StateUserOwned:
		return true
	default:
		return false
	}
}

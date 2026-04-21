package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	run               *ipc.RunInfo
	steps             []ipc.StepResultInfo
	stepFindings      map[types.StepName]string          // step name → raw findings JSON
	stepDiffs         map[types.StepName]string          // step name → raw unified diff
	findingSelections map[types.StepName]map[string]bool // step name → finding ID → selected
	findingCursor     map[types.StepName]int             // step name → current finding cursor
	logs              []string
	logPartial        string // buffered partial line (no trailing newline yet)

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
}

// NewModel creates a TUI model for the given run.
// The client should already be connected to the daemon.
func NewModel(socketPath string, client *ipc.Client, run *ipc.RunInfo) Model {
	steps := normalizePipelineSteps(run.ID, run.Steps)
	run.Steps = steps
	m := Model{
		socketPath:        socketPath,
		client:            client,
		runID:             run.ID,
		subscriptionID:    1,
		run:               run,
		done:              run.Status == types.RunCompleted || run.Status == types.RunFailed || run.Status == types.RunCancelled,
		steps:             steps,
		stepFindings:      make(map[types.StepName]string),
		stepDiffs:         make(map[types.StepName]string),
		findingSelections: make(map[types.StepName]map[string]bool),
		findingCursor:     make(map[types.StepName]int),
		stepStartTimes:    make(map[types.StepName]time.Time),
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

func normalizePipelineSteps(runID string, steps []ipc.StepResultInfo) []ipc.StepResultInfo {
	knownSteps := types.AllSteps()
	if !shouldBackfillPipelineSteps(steps, knownSteps) {
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

func shouldBackfillPipelineSteps(steps []ipc.StepResultInfo, knownSteps []types.StepName) bool {
	if len(steps) == 0 {
		return true
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
		m.applyEvent(msg.event)
		if m.done {
			return m, nil
		}
		return m, tea.Batch(m.waitForEvent(), m.startSpinnerIfNeeded())

	case subscriptionErrMsg:
		if msg.subscriptionID != m.subscriptionID {
			return m, nil
		}
		m.err = msg.err
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
		}
	}

	// Find the most relevant active step.
	for _, s := range m.steps {
		icon := stepStatusIndicator(s.Status, m.spinnerFrame)
		switch s.Status {
		case types.StepStatusRunning, types.StepStatusFixing:
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
func Run(socketPath string, client *ipc.Client, run *ipc.RunInfo, latestVersion string) error {
	model := NewModel(socketPath, client, run)
	model.latestVersion = latestVersion
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

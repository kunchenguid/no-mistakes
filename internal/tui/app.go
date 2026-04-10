package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// eventMsg wraps an IPC event received from the daemon.
type eventMsg ipc.Event

// errMsg wraps an error from async operations.
type errMsg struct{ err error }

type spinnerTickMsg struct{}

const spinnerTickInterval = 120 * time.Millisecond

func (e errMsg) Error() string { return e.err.Error() }

// connectedMsg signals that the event subscription is ready.
type connectedMsg struct {
	events    <-chan ipc.Event
	cancelSub func()
}

// Model is the root bubbletea model for the TUI.
type Model struct {
	// Connection.
	socketPath string
	client     *ipc.Client
	events     <-chan ipc.Event
	cancelSub  func()
	runID      string

	// State.
	run               *ipc.RunInfo
	steps             []ipc.StepResultInfo
	stepFindings      map[types.StepName]string          // step name → raw findings JSON
	stepDiffs         map[types.StepName]string          // step name → raw unified diff
	findingSelections map[types.StepName]map[string]bool // step name → finding ID → selected
	findingCursor     map[types.StepName]int             // step name → current finding cursor
	logs              []string

	// UI.
	width            int
	height           int
	err              error
	quitting         bool
	done             bool // run completed or failed
	showDiff         bool // toggle diff viewer
	diffOffset       int  // scroll position in diff view
	spinnerFrame     int
	spinnerScheduled bool
}

// NewModel creates a TUI model for the given run.
// The client should already be connected to the daemon.
func NewModel(socketPath string, client *ipc.Client, run *ipc.RunInfo) Model {
	m := Model{
		socketPath:        socketPath,
		client:            client,
		runID:             run.ID,
		run:               run,
		steps:             run.Steps,
		stepFindings:      make(map[types.StepName]string),
		stepDiffs:         make(map[types.StepName]string),
		findingSelections: make(map[types.StepName]map[string]bool),
		findingCursor:     make(map[types.StepName]int),
	}
	// Populate findings from initial step data (for re-attach scenarios).
	for _, s := range run.Steps {
		if s.FindingsJSON != nil && *s.FindingsJSON != "" {
			m.stepFindings[s.StepName] = *s.FindingsJSON
			if s.Status == types.StepStatusAwaitingApproval || s.Status == types.StepStatusFixReview {
				m.resetFindingSelection(s.StepName)
			}
		}
	}
	return m
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
		m.events = msg.events
		m.cancelSub = msg.cancelSub
		return m, tea.Batch(m.waitForEvent(), m.startSpinnerIfNeeded())

	case eventMsg:
		m.applyEvent(ipc.Event(msg))
		if m.done {
			return m, nil
		}
		return m, tea.Batch(m.waitForEvent(), m.startSpinnerIfNeeded())

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

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder
	showSelectionActions, allowFix := m.awaitingActionState()

	// Pipeline progress view.
	b.WriteString(renderPipelineView(m.run, m.steps, m.width, m.spinnerFrame, showSelectionActions, allowFix))

	// Babysit-specific view when babysit step is active.
	if isBabysitActive(m.steps) {
		findings := ""
		cursor := 0
		var selected map[string]bool
		if step := awaitingStep(m.steps); step != nil {
			findings = m.stepFindings[step.StepName]
			cursor = m.findingCursor[step.StepName]
			selected = m.findingSelections[step.StepName]
		}
		b.WriteString("\n\n")
		b.WriteString(renderBabysitViewWithSelection(m.run, m.steps, findings, m.logs, m.width, m.height, cursor, selected))
	} else if step := awaitingStep(m.steps); step != nil {
		// Generic findings or diff for non-babysit steps awaiting approval.
		label := stepLabel(step.StepName)
		if m.showDiff {
			if raw, ok := m.stepDiffs[step.StepName]; ok && raw != "" {
				viewHeight := m.height - 15 // reserve space for pipeline + footer
				if viewHeight < 5 {
					viewHeight = 10
				}
				b.WriteString("\n\n")
				b.WriteString(renderDiff(raw, m.width, viewHeight, m.diffOffset, label))
			}
		} else if raw, ok := m.stepFindings[step.StepName]; ok {
			// Compute max visible findings from available height.
			// Each finding is ~3 lines (gutter + description + blank separator).
			// Reserve space for summary (2 lines), severity counts (2 lines), and scroll indicators (2 lines).
			findingsHeight := m.height - 20 // reserve for pipeline, action bar, log, footer
			maxVisible := 0
			if findingsHeight > 6 {
				maxVisible = findingsHeight / 3
			}
			cursor := m.findingCursor[step.StepName]
			rendered := renderFindingsWithSelection(raw, m.width-4, cursor, m.findingSelections[step.StepName], maxVisible)
			if rendered != "" {
				boxWidth := m.width
				if boxWidth < 20 {
					boxWidth = 80
				}
				title := "Findings - " + label
				if items := m.findingItems(step.StepName); len(items) > 0 {
					title += fmt.Sprintf(" (%d/%d)", cursor+1, len(items))
				}
				b.WriteString("\n\n")
				b.WriteString(renderBox(title, rendered, boxWidth))
			}
		}
	}

	// Log tail (last 5 lines) in a box.
	if len(m.logs) > 0 {
		b.WriteString("\n\n")
		logDimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
		logGreenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
		logRedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
		start := len(m.logs) - 5
		if start < 0 {
			start = 0
		}
		var logContent strings.Builder
		for i, line := range m.logs[start:] {
			if i > 0 {
				logContent.WriteString("\n")
			}
			switch {
			case strings.HasPrefix(line, "PASS"):
				logContent.WriteString(logGreenStyle.Render(line))
			case strings.HasPrefix(line, "FAIL"):
				logContent.WriteString(logRedStyle.Render(line))
			default:
				logContent.WriteString(logDimStyle.Render(line))
			}
		}
		boxWidth := m.width
		if boxWidth < 20 {
			boxWidth = 80
		}
		b.WriteString(renderBox("Log", logContent.String(), boxWidth))
		b.WriteString("\n")
	}

	// Error display.
	if m.err != nil {
		errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
		b.WriteString("\n" + errStyle.Render("Error: "+m.err.Error()) + "\n")
	}

	// Footer.
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	boldKey := lipgloss.NewStyle().Bold(true)
	if m.done {
		b.WriteString("\n  " + boldKey.Render("q") + " " + dimStyle.Render("quit") + "\n")
	} else {
		b.WriteString("\n  " + boldKey.Render("q") + " " + dimStyle.Render("detach") + "\n")
	}

	return b.String()
}

func (m Model) awaitingActionState() (showSelectionActions bool, allowFix bool) {
	step := awaitingStep(m.steps)
	if step == nil {
		return false, false
	}
	items := m.findingItems(step.StepName)
	if len(items) == 0 {
		return false, false
	}
	selected, ok := m.findingSelections[step.StepName]
	if !ok {
		return true, true
	}
	return true, len(selected) > 0
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

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "d":
		if awaitingStep(m.steps) != nil {
			m.showDiff = !m.showDiff
			m.diffOffset = 0
		}
		return m, nil

	case "j", "down":
		if m.showDiff {
			m.diffOffset++
		} else if step := awaitingStep(m.steps); step != nil {
			m.moveFindingCursor(step.StepName, 1)
		}
		return m, nil

	case "k", "up":
		if m.showDiff && m.diffOffset > 0 {
			m.diffOffset--
		} else if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.moveFindingCursor(step.StepName, -1)
			}
		}
		return m, nil

	case " ":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.toggleCurrentFinding(step.StepName)
			}
		}
		return m, nil

	case "A":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.selectAllFindings(step.StepName)
			}
		}
		return m, nil

	case "N":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.clearAllFindings(step.StepName)
			}
		}
		return m, nil

	case "a":
		return m, m.respondCmd(types.ActionApprove)
	case "f":
		return m, m.respondCmd(types.ActionFix)
	case "s":
		return m, m.respondCmd(types.ActionSkip)
	case "x":
		return m, m.respondCmd(types.ActionAbort)
	}
	return m, nil
}

func (m Model) respondCmd(action types.ApprovalAction) tea.Cmd {
	step := awaitingStep(m.steps)
	if step == nil {
		return nil
	}
	if action == types.ActionFix {
		ids := m.selectedFindingIDs(step.StepName)
		if len(ids) == 0 && len(m.findingItems(step.StepName)) > 0 {
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

func (m Model) subscribeCmd() tea.Cmd {
	return func() tea.Msg {
		events, cancel, err := ipc.Subscribe(m.socketPath, &ipc.SubscribeParams{
			RunID: m.runID,
		})
		if err != nil {
			return errMsg{fmt.Errorf("subscribe: %w", err)}
		}
		return connectedMsg{events: events, cancelSub: cancel}
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
			return errMsg{fmt.Errorf("event stream closed")}
		}
		return eventMsg(event)
	}
}

func (m *Model) applyEvent(event ipc.Event) {
	switch event.Type {
	case ipc.EventRunUpdated, ipc.EventRunCreated:
		if event.Status != nil {
			m.run.Status = types.RunStatus(*event.Status)
		}

	case ipc.EventRunCompleted:
		if event.Status != nil {
			m.run.Status = types.RunStatus(*event.Status)
		}
		m.done = true

	case ipc.EventStepStarted:
		if event.StepName != nil {
			m.updateStepStatus(*event.StepName, types.StepStatusRunning)
		}

	case ipc.EventStepCompleted:
		if event.StepName != nil && event.Status != nil {
			m.updateStepStatus(*event.StepName, types.StepStatus(*event.Status))
		}
		if event.StepName != nil && event.Findings != nil && *event.Findings != "" {
			m.stepFindings[*event.StepName] = *event.Findings
			if event.Status != nil && (types.StepStatus(*event.Status) == types.StepStatusAwaitingApproval || types.StepStatus(*event.Status) == types.StepStatusFixReview) {
				m.resetFindingSelection(*event.StepName)
			}
		}
		if event.StepName != nil && event.Diff != nil && *event.Diff != "" {
			m.stepDiffs[*event.StepName] = *event.Diff
			m.showDiff = false
			m.diffOffset = 0
		}

	case ipc.EventLogChunk:
		if event.Content != nil && *event.Content != "" {
			lines := strings.Split(strings.TrimRight(*event.Content, "\n"), "\n")
			m.logs = append(m.logs, lines...)
			// Keep last 100 lines to bound memory.
			if len(m.logs) > 100 {
				m.logs = m.logs[len(m.logs)-100:]
			}
		}
	}
}

func (m *Model) updateStepStatus(name types.StepName, status types.StepStatus) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].Status = status
			return
		}
	}
}

func (m *Model) findingItems(step types.StepName) []finding {
	raw := m.stepFindings[step]
	f, err := parseFindings(raw)
	if err != nil || f == nil {
		return nil
	}
	return f.Items
}

func (m *Model) ensureFindingSelection(step types.StepName) {
	if m.findingSelections == nil {
		m.findingSelections = make(map[types.StepName]map[string]bool)
	}
	if m.findingCursor == nil {
		m.findingCursor = make(map[types.StepName]int)
	}
	if _, ok := m.findingSelections[step]; ok {
		return
	}
	m.resetFindingSelection(step)
}

func (m *Model) resetFindingSelection(step types.StepName) {
	if m.findingSelections == nil {
		m.findingSelections = make(map[types.StepName]map[string]bool)
	}
	if m.findingCursor == nil {
		m.findingCursor = make(map[types.StepName]int)
	}
	selected := make(map[string]bool)
	for _, item := range m.findingItems(step) {
		if item.ID != "" {
			selected[item.ID] = true
		}
	}
	m.findingSelections[step] = selected
	m.findingCursor[step] = 0
}

func (m *Model) selectedFindingIDs(step types.StepName) []string {
	selected := m.findingSelections[step]
	if len(selected) == 0 {
		return nil
	}
	var ids []string
	for _, item := range m.findingItems(step) {
		if selected[item.ID] {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func (m *Model) moveFindingCursor(step types.StepName, delta int) {
	items := m.findingItems(step)
	if len(items) == 0 {
		return
	}
	cur := m.findingCursor[step] + delta
	if cur < 0 {
		cur = 0
	}
	if cur >= len(items) {
		cur = len(items) - 1
	}
	m.findingCursor[step] = cur
}

func (m *Model) toggleCurrentFinding(step types.StepName) {
	items := m.findingItems(step)
	if len(items) == 0 {
		return
	}
	m.ensureFindingSelection(step)
	cur := m.findingCursor[step]
	if cur < 0 || cur >= len(items) {
		return
	}
	id := items[cur].ID
	if id == "" {
		return
	}
	m.findingSelections[step][id] = !m.findingSelections[step][id]
	if !m.findingSelections[step][id] {
		delete(m.findingSelections[step], id)
	}
	if m.findingSelections[step] == nil {
		m.findingSelections[step] = make(map[string]bool)
	}
	if m.findingSelections[step][id] {
		return
	}
}

func (m *Model) selectAllFindings(step types.StepName) {
	m.resetFindingSelection(step)
}

func (m *Model) clearAllFindings(step types.StepName) {
	m.ensureFindingSelection(step)
	m.findingSelections[step] = make(map[string]bool)
}

// Run starts the TUI program.
func Run(socketPath string, client *ipc.Client, run *ipc.RunInfo) error {
	model := NewModel(socketPath, client, run)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

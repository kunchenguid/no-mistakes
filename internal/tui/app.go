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

	// Timing.
	stepStartTimes map[types.StepName]time.Time // when each step started running

	// UI.
	width            int
	height           int
	err              error
	quitting         bool
	done             bool // run completed or failed
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
		stepStartTimes:    make(map[types.StepName]time.Time),
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
	showSelectionActions, allowFix, selectedCount, totalCount := m.awaitingActionState()

	// Pipeline progress view.
	// Compute elapsed times for running steps so they display live durations.
	pipelineSteps := m.stepsWithRunningElapsed()
	b.WriteString(renderPipelineView(m.run, pipelineSteps, m.width, m.spinnerFrame, m.height))

	// Outcome banner when run is done.
	if banner := renderOutcomeBanner(m.run, m.steps); banner != "" {
		b.WriteString("\n")
		b.WriteString(banner)
	}

	// Action bar between pipeline box and findings/diff per DESIGN.md.
	hasDiff := false
	if step := awaitingStep(m.steps); step != nil {
		raw, ok := m.stepDiffs[step.StepName]
		hasDiff = ok && raw != ""
	}
	if actionBar := renderActionBar(m.steps, showSelectionActions, allowFix, m.showDiff, selectedCount, totalCount, m.confirmAbort, hasDiff); actionBar != "" {
		b.WriteString("\n")
		b.WriteString(actionBar)
	}

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
				// Build finding context for diff view header.
				findingCtx := ""
				if items := m.findingItems(step.StepName); len(items) > 0 {
					cur := m.findingCursor[step.StepName]
					if cur >= 0 && cur < len(items) {
						item := items[cur]
						ref := item.File
						if item.Line > 0 {
							ref = fmt.Sprintf("%s:%d", item.File, item.Line)
						}
						findingCtx = fmt.Sprintf("%s %s  %s  (%d/%d)", severityIcon(item.Severity), ref, item.Description, cur+1, len(items))
					}
				}
				b.WriteString("\n\n")
				b.WriteString(renderDiff(raw, m.width, viewHeight, m.diffOffset, label, findingCtx))
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
			rendered, scrollFooter := renderFindingsWithSelection(raw, m.width-4, cursor, m.findingSelections[step.StepName], maxVisible)
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
				b.WriteString(renderBoxWithFooter(title, rendered, boxWidth, scrollFooter))
			}
		}
	}

	// Log tail in a box - adaptive line count based on terminal height.
	// height >= 30: 5 lines, height 20-29: 3 lines, height < 20: hidden.
	// Also hidden when babysit is active (log context integrated into babysit box).
	logLines := 5
	if m.height > 0 && m.height < 30 {
		logLines = 3
	}
	if m.height > 0 && m.height < 20 {
		logLines = 0
	}
	if len(m.logs) > 0 && logLines > 0 && !isBabysitActive(m.steps) {
		b.WriteString("\n\n")
		start := len(m.logs) - logLines
		if start < 0 {
			start = 0
		}
		boxWidth := m.width
		if boxWidth < 20 {
			boxWidth = 80
		}
		contentWidth := boxWidth - 4 // 2 border + 2 padding
		var logContent strings.Builder
		for i, line := range m.logs[start:] {
			if i > 0 {
				logContent.WriteString("\n")
			}
			line, _ = cutText(line, contentWidth)
			logContent.WriteString(styleLogLine(line))
		}
		b.WriteString(renderBox("Log", logContent.String(), boxWidth))
		b.WriteString("\n")
	}

	// Help overlay.
	if m.showHelp {
		boxWidth := m.width
		if boxWidth < 20 {
			boxWidth = 80
		}
		b.WriteString("\n\n")
		b.WriteString(renderHelpOverlay(boxWidth, awaitingStep(m.steps) != nil, m.showDiff, hasDiff, m.done))
	}

	// Error display in a box per DESIGN.md.
	if m.err != nil {
		boxWidth := m.width
		if boxWidth < 20 {
			boxWidth = 80
		}
		contentWidth := boxWidth - 4 // 2 border + 2 padding
		errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
		// Truncate each line of the error message to fit inside the box.
		errLines := strings.Split(m.err.Error(), "\n")
		var errContent strings.Builder
		for i, line := range errLines {
			if i > 0 {
				errContent.WriteString("\n")
			}
			line, _ = cutText(line, contentWidth)
			errContent.WriteString(errStyle.Render(line))
		}
		b.WriteString("\n\n")
		b.WriteString(renderBox("Error", errContent.String(), boxWidth))
	}

	// Footer.
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	boldKey := lipgloss.NewStyle().Bold(true)
	qLabel := "detach"
	if m.done {
		qLabel = "quit"
	}
	helpLabel := "help"
	if m.showHelp {
		helpLabel = "close"
	}
	b.WriteString("\n  " + boldKey.Render("q") + " " + dimStyle.Render(qLabel) + "  " + boldKey.Render("?") + " " + dimStyle.Render(helpLabel) + "\n")

	return b.String()
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
		case types.StepStatusRunning, types.StepStatusFixing:
			if startTime, ok := m.stepStartTimes[steps[i].StepName]; ok {
				elapsed := int64(time.Since(startTime).Milliseconds())
				steps[i].DurationMS = &elapsed
			}
		}
	}
	return steps
}

func (m Model) awaitingActionState() (showSelectionActions bool, allowFix bool, selectedCount int, totalCount int) {
	step := awaitingStep(m.steps)
	if step == nil {
		return false, false, 0, 0
	}
	items := m.findingItems(step.StepName)
	if len(items) == 0 {
		return false, false, 0, 0
	}
	totalCount = len(items)
	selected, ok := m.findingSelections[step.StepName]
	if !ok {
		return true, true, totalCount, totalCount
	}
	selectedCount = len(selected)
	return true, selectedCount > 0, selectedCount, totalCount
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
	key := msg.String()

	// Reset abort confirmation on any key except 'x'.
	if key != "x" {
		m.confirmAbort = false
	}

	// Auto-dismiss help on any key except ? (toggle) and esc (handled below).
	if m.showHelp && key != "?" && key != "esc" {
		m.showHelp = false
	}

	switch key {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "?":
		m.showHelp = !m.showHelp
		return m, nil

	case "esc":
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		if m.showDiff {
			m.showDiff = false
			m.diffOffset = 0
			return m, nil
		}

	case "d":
		if step := awaitingStep(m.steps); step != nil {
			if raw, ok := m.stepDiffs[step.StepName]; ok && raw != "" {
				m.showDiff = !m.showDiff
				if m.showDiff {
					m.diffOffset = m.diffOffsetForCurrentFinding(step.StepName)
				} else {
					m.diffOffset = 0
				}
			}
		}
		return m, nil

	case "n":
		if m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.moveFindingCursor(step.StepName, 1)
				m.diffOffset = m.diffOffsetForCurrentFinding(step.StepName)
			}
		}
		return m, nil

	case "p":
		if m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.moveFindingCursor(step.StepName, -1)
				m.diffOffset = m.diffOffsetForCurrentFinding(step.StepName)
			}
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

	case "g", "home":
		if m.showDiff {
			m.diffOffset = 0
		} else if step := awaitingStep(m.steps); step != nil {
			m.findingCursor[step.StepName] = 0
		}
		return m, nil

	case "G", "end":
		if m.showDiff {
			m.diffOffset = 1<<31 - 1 // large value, renderDiff clamps
		} else if step := awaitingStep(m.steps); step != nil {
			items := m.findingItems(step.StepName)
			if len(items) > 0 {
				m.findingCursor[step.StepName] = len(items) - 1
			}
		}
		return m, nil

	case "ctrl+d":
		if m.showDiff {
			half := (m.height - 15) / 2
			if half < 1 {
				half = 1
			}
			m.diffOffset += half
		} else if step := awaitingStep(m.steps); step != nil {
			half := (m.height - 20) / 3 / 2
			if half < 1 {
				half = 1
			}
			m.moveFindingCursor(step.StepName, half)
		}
		return m, nil

	case "ctrl+u":
		if m.showDiff {
			half := (m.height - 15) / 2
			if half < 1 {
				half = 1
			}
			m.diffOffset -= half
			if m.diffOffset < 0 {
				m.diffOffset = 0
			}
		} else if step := awaitingStep(m.steps); step != nil {
			half := (m.height - 20) / 3 / 2
			if half < 1 {
				half = 1
			}
			m.moveFindingCursor(step.StepName, -half)
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
		if m.confirmAbort {
			m.confirmAbort = false
			return m, m.respondCmd(types.ActionAbort)
		}
		m.confirmAbort = true
		return m, nil
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
			m.stepStartTimes[*event.StepName] = time.Now()
		}

	case ipc.EventStepCompleted:
		if event.StepName != nil && event.Status != nil {
			m.updateStepStatus(*event.StepName, types.StepStatus(*event.Status))
		}
		// Compute and persist final duration from tracked start time so the
		// completed step continues to display its elapsed time.
		if event.StepName != nil {
			if startTime, ok := m.stepStartTimes[*event.StepName]; ok {
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

func (m *Model) setStepDuration(name types.StepName, durationMS *int64) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].DurationMS = durationMS
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

// diffOffsetForCurrentFinding returns the diff scroll offset that corresponds
// to the current finding's file:line. Returns 0 if no match.
func (m Model) diffOffsetForCurrentFinding(step types.StepName) int {
	items := m.findingItems(step)
	if len(items) == 0 {
		return 0
	}
	cursor := m.findingCursor[step]
	if cursor < 0 || cursor >= len(items) {
		return 0
	}
	item := items[cursor]
	if item.File == "" {
		return 0
	}
	raw := m.stepDiffs[step]
	if raw == "" {
		return 0
	}
	lines := parseDiffLines(raw)
	return findDiffOffset(lines, item.File, item.Line)
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

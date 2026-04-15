package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// eventMsg wraps an IPC event received from the daemon.
type eventMsg struct {
	event          ipc.Event
	subscriptionID uint64
}

// errMsg wraps an error from async operations.
type errMsg struct{ err error }

type subscriptionErrMsg struct {
	err            error
	subscriptionID uint64
}

// rerunStartedMsg switches the TUI onto a newly created rerun.
type rerunStartedMsg struct {
	run       *ipc.RunInfo
	requestID uint64
}

type rerunErrMsg struct {
	err       error
	requestID uint64
}

type spinnerTickMsg struct{}

const spinnerTickInterval = 120 * time.Millisecond

const (
	responsiveLayoutMinWidth = 100
	responsiveLayoutGap      = 2
	responsiveLeftMinWidth   = 38
	responsiveLeftMaxWidth   = 48
	responsiveRightMinWidth  = 48

	// cappedPipelineHeight is the height passed to renderPipelineView when
	// an overlay (help) is active in non-responsive (stacked) layout.
	// Kept below 30 to suppress connector lines and save vertical space
	// for the overlay that stacks below.
	cappedPipelineHeight = 29
)

var runBrowserCommand = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func (e errMsg) Error() string { return e.err.Error() }

// connectedMsg signals that the event subscription is ready.
type connectedMsg struct {
	events         <-chan ipc.Event
	cancelSub      func()
	subscriptionID uint64
}

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
	m := Model{
		socketPath:        socketPath,
		client:            client,
		runID:             run.ID,
		subscriptionID:    1,
		run:               run,
		done:              run.Status == types.RunCompleted || run.Status == types.RunFailed || run.Status == types.RunCancelled,
		steps:             run.Steps,
		stepFindings:      make(map[types.StepName]string),
		stepDiffs:         make(map[types.StepName]string),
		findingSelections: make(map[types.StepName]map[string]bool),
		findingCursor:     make(map[types.StepName]int),
		stepStartTimes:    make(map[types.StepName]time.Time),
	}
	// Populate findings and start times from initial step data (for re-attach scenarios).
	for _, s := range run.Steps {
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

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	showSelectionActions, allowFix, selectedCount, totalCount := m.awaitingActionState()
	hasCI := isCIActive(m.steps)
	compact := m.height > 0 && m.height < 24
	sectionGap := "\n\n"
	sectionGapHeight := 2
	if compact {
		sectionGap = "\n"
		sectionGapHeight = 1
	}

	useResponsiveLayout := shouldUseResponsiveLayout(m.width, hasResponsiveSidebarContent(m))
	leftWidth := m.width
	rightWidth := m.width
	if useResponsiveLayout {
		leftWidth, rightWidth = responsiveColumnWidths(m.width)
	}

	// Pipeline progress view.
	// Compute elapsed times for running steps so they display live durations.
	pipelineSteps := m.stepsWithRunningElapsed()
	pipelineHeight := m.height
	if !useResponsiveLayout && (m.showHelp || hasCI) && (pipelineHeight == 0 || pipelineHeight >= 30) {
		pipelineHeight = cappedPipelineHeight
	}
	pipelineView := renderPipelineView(m.run, pipelineSteps, leftWidth, m.spinnerFrame, pipelineHeight)
	banner := renderOutcomeBanner(m.run, m.steps)

	// Action bar between pipeline box and findings/diff per DESIGN.md.
	hasDiff := false
	if step := awaitingStep(m.steps); step != nil {
		raw, ok := m.stepDiffs[step.StepName]
		hasDiff = ok && raw != ""
	}
	actionBar := renderActionBar(m.steps, showSelectionActions, allowFix, m.showDiff, selectedCount, totalCount, m.confirmAbort, hasDiff)

	footer := renderFooter(m.done, m.showHelp, m.confirmAbort, m.run, m.latestVersion, m.width)
	contentBudget := -1
	if m.height > 0 {
		baseSections := []string{}
		if useResponsiveLayout {
			if actionBar != "" {
				baseSections = append(baseSections, actionBar)
			}
		} else {
			baseSections = append(baseSections, pipelineView)
			if banner != "" {
				baseSections = append(baseSections, banner)
			}
			if actionBar != "" {
				baseSections = append(baseSections, actionBar)
			}
		}
		contentBudget = m.height - sectionsHeight(baseSections, sectionGapHeight)
		contentBudget -= sectionGapHeight + lipgloss.Height(footer)
		if contentBudget < 0 {
			contentBudget = 0
		}
	}

	var extraSections []string
	appendExtraSection := func(section string) bool {
		if section == "" {
			return false
		}
		if contentBudget >= 0 {
			needed := lipgloss.Height(section)
			if len(extraSections) > 0 {
				needed += sectionGapHeight
			}
			if needed > contentBudget {
				return false
			}
			contentBudget -= needed
		}
		extraSections = append(extraSections, section)
		return true
	}

	if m.err != nil {
		appendExtraSection(renderErrorBox(m.err, rightWidth))
	}

	// CI-specific view when CI step is active.
	if !m.showHelp && hasCI {
		findings := ""
		cursor := 0
		var selected map[string]bool
		if step := awaitingStep(m.steps); step != nil {
			findings = m.stepFindings[step.StepName]
			cursor = m.findingCursor[step.StepName]
			selected = m.findingSelections[step.StepName]
		}
		ciHeight := -1
		if m.height > 0 {
			ciHeight = m.height
		}
		if contentBudget >= 0 {
			ciHeight = contentBudget
		}
		appendExtraSection(renderCIViewWithSelection(m.run, m.steps, findings, m.logs, rightWidth, ciHeight, cursor, selected))
	} else if !m.showHelp {
		if step := awaitingStep(m.steps); step != nil {
			// Generic findings or diff for non-CI steps awaiting approval.
			label := stepLabel(step.StepName)
			if m.showDiff {
				if raw, ok := m.stepDiffs[step.StepName]; ok && raw != "" {
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
					viewHeight := m.height - 15
					if contentBudget >= 0 {
						fixedLines := 4
						if findingCtx != "" {
							fixedLines++
						}
						viewHeight = contentBudget - fixedLines
					}
					if viewHeight > 0 {
						appendExtraSection(renderDiff(raw, rightWidth, viewHeight, m.diffOffset, label, findingCtx))
					}
				}
			} else if raw, ok := m.stepFindings[step.StepName]; ok {
				cursor := m.findingCursor[step.StepName]
				boxHeight := m.height
				if contentBudget >= 0 {
					boxHeight = contentBudget
				}
				appendExtraSection(renderFindingsBoxForHeight(raw, rightWidth, cursor, m.findingSelections[step.StepName], boxHeight))
			}
		}
	}

	// Log tail in a box - adaptive line count based on terminal height.
	// In responsive layout with no other right-column content, expand to
	// fill the remaining vertical budget so the log panel matches the
	// pipeline panel height. In stacked layout, use the remaining content
	// budget so the log box can consume the available terminal height.
	// Also hidden when CI is active (log context integrated into CI box).
	logLines := 5
	if !m.showHelp && contentBudget > 0 {
		if useResponsiveLayout {
			if len(extraSections) == 0 {
				logLines = contentBudget - 2 // subtract box borders
				if actionBar != "" {
					logLines -= sectionGapHeight
				}
			}
		} else {
			logLines = contentBudget - 2 // subtract box borders
		}
	} else if m.height > 0 && m.height < 30 {
		logLines = 3
	}
	if m.height > 0 && m.height < 20 {
		logLines = 0
	}
	if len(m.logs) > 0 && logLines > 0 && !hasCI {
		appendExtraSection(renderLogBox(m.logs, rightWidth, logLines, contentBudget))
	}

	if m.showHelp {
		boxWidth := rightWidth
		if boxWidth < 20 {
			boxWidth = 80
		}
		appendExtraSection(renderHelpOverlay(boxWidth, m.run, awaitingStep(m.steps) != nil, m.showDiff, hasDiff, m.done))
	}

	if useResponsiveLayout {
		leftSections := []string{pipelineView}
		if banner != "" {
			leftSections = append(leftSections, banner)
		}
		rightSections := make([]string, 0, len(extraSections)+1)
		if actionBar != "" {
			rightSections = append(rightSections, actionBar)
		}
		rightSections = append(rightSections, extraSections...)
		columns := renderResponsiveColumns(joinSections(leftSections, sectionGap), joinSections(rightSections, sectionGap), leftWidth, rightWidth, responsiveLayoutGap)
		return setTerminalTitle(m.terminalTitle()) + joinSections([]string{columns, footer}, sectionGap)
	}

	sections := []string{pipelineView}
	if banner != "" {
		sections = append(sections, banner)
	}
	if actionBar != "" {
		sections = append(sections, actionBar)
	}
	sections = append(sections, extraSections...)
	sections = append(sections, footer)
	return setTerminalTitle(m.terminalTitle()) + joinSections(sections, sectionGap)
}

func renderFindingsBoxForHeight(raw string, width int, cursor int, selected map[string]bool, boxHeight int) string {
	if boxHeight > 0 && boxHeight < 3 {
		return ""
	}
	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	contentHeight := 0
	if boxHeight > 0 {
		contentHeight = boxHeight - 2
	}

	f, err := parseFindings(raw)
	if err != nil || f == nil {
		return ""
	}

	// Build styled title: "Findings - E 2 W 2 I 2" with colorized severity counts.
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiCyan))
	styledTitle := titleStyle.Render("Findings")
	counts := map[string]int{}
	for _, item := range f.Items {
		counts[item.Severity]++
	}
	if len(counts) > 0 {
		styledTitle += titleStyle.Render(" -")
		for _, sev := range []string{"error", "warning", "info"} {
			if c, ok := counts[sev]; ok {
				styledTitle += " " + severityStyle(sev).Render(fmt.Sprintf("%s %d", severityIcon(sev), c))
			}
		}
	}

	contentWidth := boxWidth - 4
	if contentWidth < 1 {
		contentWidth = 1
	}
	var rendered string
	var scrollFooter string
	if contentHeight > 0 {
		rendered, scrollFooter = renderParsedFindingsHeight(f, contentWidth, cursor, selected, contentHeight)
	} else {
		rendered, scrollFooter = renderParsedFindingsViewport(f, contentWidth, cursor, selected, 0)
	}
	if rendered == "" {
		return ""
	}
	return renderBoxWithStyledTitle(styledTitle, rendered, boxWidth, scrollFooter)
}

func renderLogBox(logs []string, width int, logLines int, remainingBudget int) string {
	if len(logs) == 0 || logLines <= 0 {
		return ""
	}
	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	if remainingBudget >= 0 {
		maxLogLines := remainingBudget - 2
		if maxLogLines <= 0 {
			return ""
		}
		if logLines > maxLogLines {
			logLines = maxLogLines
		}
	}
	contentWidth := boxWidth - 4
	renderedLines := renderLogTail(logs, contentWidth, logLines)
	if len(renderedLines) == 0 {
		return ""
	}
	var logContent strings.Builder
	logContent.WriteString(strings.Join(renderedLines, "\n"))
	return renderBox("Log", logContent.String(), boxWidth)
}

func renderErrorBox(err error, width int) string {
	if err == nil {
		return ""
	}
	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	contentWidth := boxWidth - 4 // 2 border + 2 padding
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	errLines := strings.Split(err.Error(), "\n")
	var errContent strings.Builder
	for i, line := range errLines {
		if i > 0 {
			errContent.WriteString("\n")
		}
		line, _ = cutText(line, contentWidth)
		errContent.WriteString(errStyle.Render(line))
	}
	return renderBox("Error", errContent.String(), boxWidth)
}

func renderFooter(done bool, showHelp bool, confirmAbort bool, run *ipc.RunInfo, latestVersion string, width int) string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))
	boldKey := lipgloss.NewStyle().Bold(true)
	qLabel := "detach"
	if done {
		qLabel = "quit"
	}
	helpLabel := "help"
	if showHelp {
		helpLabel = "close"
	}
	left := "  " + boldKey.Render("q") + " " + dimStyle.Render(qLabel)
	if !done {
		xLabel := "abort"
		if confirmAbort {
			xLabel = "again to abort"
		}
		left += "  " + boldKey.Render("x") + " " + dimStyle.Render(xLabel)
	}
	left += "  " + boldKey.Render("?") + " " + dimStyle.Render(helpLabel)
	if canRerun(run) {
		left += "  " + boldKey.Render("r") + " " + dimStyle.Render("rerun")
	}

	var prURL *string
	if run != nil {
		prURL = run.PRURL
	}

	rightParts := []string{}
	if latestVersion != "" {
		rightParts = append(rightParts, warnStyle.Render(latestVersion+" available"))
	}
	if prURL == nil || *prURL == "" {
		if len(rightParts) == 0 {
			return left
		}
		right := strings.Join(rightParts, "  ")
		gap := width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 2 {
			return left
		}
		return left + strings.Repeat(" ", gap) + right
	}

	left += "  " + boldKey.Render("o") + " " + dimStyle.Render("open PR")
	leftWidth := lipgloss.Width(left)
	reservedRightWidth := 0
	if len(rightParts) > 0 {
		reservedRightWidth = lipgloss.Width(strings.Join(rightParts, "  ")) + 2
	}
	available := width - leftWidth - reservedRightWidth - 2
	prText := *prURL
	if available < lipgloss.Width(prText) {
		prText = shortPRLabel(*prURL)
	}
	if available >= lipgloss.Width(prText) {
		rightParts = append([]string{dimStyle.Render(prText)}, rightParts...)
	}
	if len(rightParts) == 0 {
		return left
	}
	right := strings.Join(rightParts, "  ")
	gap := width - leftWidth - lipgloss.Width(right)
	if gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
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

// shortPRLabel extracts a compact label like "PR #42" from a PR URL.
func shortPRLabel(url string) string {
	// GitHub: .../pull/42, GitLab: .../merge_requests/42
	for _, prefix := range []string{"/pull/", "/merge_requests/"} {
		if idx := strings.LastIndex(url, prefix); idx >= 0 {
			num := url[idx+len(prefix):]
			if num != "" {
				label := "PR"
				if prefix == "/merge_requests/" {
					label = "MR"
				}
				return label + " #" + num
			}
		}
	}
	return url
}

func joinSections(sections []string, gap string) string {
	filtered := make([]string, 0, len(sections))
	for _, section := range sections {
		if section != "" {
			filtered = append(filtered, section)
		}
	}
	return strings.Join(filtered, gap)
}

func hasResponsiveSidebarContent(m Model) bool {
	if m.err != nil || m.showHelp || isCIActive(m.steps) {
		return true
	}
	if awaitingStep(m.steps) != nil {
		return true
	}
	return len(m.logs) > 0
}

func shouldUseResponsiveLayout(width int, hasSidebarContent bool) bool {
	if !hasSidebarContent || width < responsiveLayoutMinWidth {
		return false
	}
	leftWidth, rightWidth := responsiveColumnWidths(width)
	return leftWidth >= responsiveLeftMinWidth && rightWidth >= responsiveRightMinWidth
}

func responsiveColumnWidths(width int) (int, int) {
	leftWidth := width / 3
	if leftWidth < responsiveLeftMinWidth {
		leftWidth = responsiveLeftMinWidth
	}
	if leftWidth > responsiveLeftMaxWidth {
		leftWidth = responsiveLeftMaxWidth
	}
	rightWidth := width - leftWidth - responsiveLayoutGap
	if rightWidth < responsiveRightMinWidth {
		rightWidth = responsiveRightMinWidth
		leftWidth = width - rightWidth - responsiveLayoutGap
	}
	return leftWidth, rightWidth
}

func renderResponsiveColumns(left, right string, leftWidth, rightWidth, gap int) string {
	if right == "" {
		return left
	}
	leftLines := strings.Split(left, "\n")
	rightLines := strings.Split(right, "\n")
	maxLines := len(leftLines)
	if len(rightLines) > maxLines {
		maxLines = len(rightLines)
	}

	leftStyle := lipgloss.NewStyle().Width(leftWidth)
	rightStyle := lipgloss.NewStyle().Width(rightWidth)
	gapStr := strings.Repeat(" ", gap)

	var b strings.Builder
	for i := 0; i < maxLines; i++ {
		leftLine := ""
		if i < len(leftLines) {
			leftLine = leftLines[i]
		}
		rightLine := ""
		if i < len(rightLines) {
			rightLine = rightLines[i]
		}
		b.WriteString(leftStyle.Render(leftLine))
		b.WriteString(gapStr)
		b.WriteString(rightStyle.Render(rightLine))
		if i < maxLines-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func sectionsHeight(sections []string, gapHeight int) int {
	count := 0
	height := 0
	for _, section := range sections {
		if section == "" {
			continue
		}
		if count > 0 {
			height += gapHeight
		}
		height += lipgloss.Height(section)
		count++
	}
	return height
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
		return m, tea.Sequence(tea.SetWindowTitle(""), tea.Quit)

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
				m.moveFindingCursor(step.StepName, 1)
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
	case "o":
		if m.run != nil && m.run.PRURL != nil && *m.run.PRURL != "" {
			return m, openBrowserCmd(*m.run.PRURL)
		}
		return m, nil
	case "r":
		if m.rerunPending || !canRerun(m.run) {
			return m, nil
		}
		m.rerunPending = true
		m.rerunRequestID++
		return m, m.rerunCmd(m.rerunRequestID)
	case "x":
		if m.done || m.run == nil {
			return m, nil
		}
		if m.confirmAbort {
			m.confirmAbort = false
			return m, m.cancelRunCmd()
		}
		m.confirmAbort = true
		return m, nil
	}
	return m, nil
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

func (m *Model) applyEvent(event ipc.Event) {
	switch event.Type {
	case ipc.EventRunUpdated, ipc.EventRunCreated:
		m.err = nil
		if event.Status != nil {
			m.run.Status = types.RunStatus(*event.Status)
		}
		if event.PRURL != nil {
			m.run.PRURL = event.PRURL
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
		m.flushPartialLog()
		m.done = true

	case ipc.EventStepStarted:
		m.err = nil
		if event.StepName != nil {
			m.updateStepStatus(*event.StepName, types.StepStatusRunning)
			m.stepStartTimes[*event.StepName] = time.Now()
		}

	case ipc.EventStepCompleted:
		m.err = nil
		m.flushPartialLog()
		if event.StepName != nil && event.Status != nil {
			m.updateStepStatus(*event.StepName, types.StepStatus(*event.Status))
		}
		if event.StepName != nil && event.Error != nil {
			m.setStepError(*event.StepName, event.Error)
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

func Run(socketPath string, client *ipc.Client, run *ipc.RunInfo, latestVersion string) error {
	model := NewModel(socketPath, client, run)
	model.latestVersion = latestVersion
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

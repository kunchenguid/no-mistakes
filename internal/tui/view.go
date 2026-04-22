package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

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

	// Modal editor takes priority over findings/logs so it always renders
	// when active. Bypass the content budget so it never gets dropped on
	// cramped terminals; the editor is user-driven and must stay visible.
	if m.editorActive() {
		boxWidth := rightWidth
		if boxWidth < 40 {
			boxWidth = 80
		}
		var section string
		switch m.editor.kind {
		case editorInstruction:
			section = m.renderInstructionEditor(boxWidth)
		case editorAddFinding:
			section = m.renderAddFindingEditor(boxWidth)
		}
		if section != "" {
			extraSections = append(extraSections, section)
			if contentBudget > 0 {
				contentBudget -= lipgloss.Height(section)
				if contentBudget < 0 {
					contentBudget = 0
				}
			}
		}
	}

	// CI-specific view when CI step is active.
	if !m.showHelp && !m.editorActive() && hasCI {
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
	} else if !m.showHelp && !m.editorActive() {
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
			} else if _, ok := m.stepFindings[step.StepName]; ok || len(m.addedFindings[step.StepName]) > 0 {
				cursor := m.findingCursor[step.StepName]
				boxHeight := m.height
				if contentBudget >= 0 {
					boxHeight = contentBudget
				}
				raw := m.combinedFindingsJSON(step.StepName)
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
	if m.editorActive() {
		logLines = 0
	}
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

// shortPRLabel extracts a compact label like "PR #42" from a PR URL.
func shortPRLabel(url string) string {
	// GitHub: .../pull/42, GitLab: .../merge_requests/42, Bitbucket: .../pull-requests/42
	for _, prefix := range []string{"/pull/", "/merge_requests/", "/pull-requests/"} {
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

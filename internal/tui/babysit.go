package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// isBabysitActive returns true if the babysit step is currently active
// (running, fixing, or awaiting approval).
func isBabysitActive(steps []ipc.StepResultInfo) bool {
	for _, s := range steps {
		if s.StepName == types.StepBabysit {
			switch s.Status {
			case types.StepStatusRunning, types.StepStatusFixing,
				types.StepStatusAwaitingApproval, types.StepStatusFixReview:
				return true
			}
		}
	}
	return false
}

// babysitStepStatus returns the current status of the babysit step.
func babysitStepStatus(steps []ipc.StepResultInfo) types.StepStatus {
	for _, s := range steps {
		if s.StepName == types.StepBabysit {
			return s.Status
		}
	}
	return types.StepStatusPending
}

// extractPRFromLogs extracts the PR number from babysit log messages.
// Looks for the "babysitting PR #42" pattern. Returns empty if not found.
func extractPRFromLogs(logs []string) string {
	for _, line := range logs {
		if idx := strings.Index(line, "PR #"); idx >= 0 {
			rest := line[idx+4:]
			end := strings.IndexAny(rest, " ()\n")
			if end < 0 {
				end = len(rest)
			}
			num := rest[:end]
			if num != "" {
				return num
			}
		}
	}
	return ""
}

// babysitActivity summarizes what the babysit step has been doing based on logs.
type babysitActivity struct {
	CIFixes    int
	AutoFixing bool
	LastEvent  string
}

// parseBabysitActivity extracts structured activity from babysit log messages.
func parseBabysitActivity(logs []string) babysitActivity {
	var a babysitActivity
	for _, line := range logs {
		switch {
		case strings.Contains(line, "CI failures detected"):
			a.CIFixes++
			a.AutoFixing = true
			a.LastEvent = line
		case strings.Contains(line, "committed and pushed fixes"):
			a.AutoFixing = false
			a.LastEvent = line
		case strings.Contains(line, "running agent to fix CI"):
			a.AutoFixing = true
			a.LastEvent = line
		case strings.Contains(line, "babysitting PR"):
			a.LastEvent = line
		case strings.Contains(line, "comment"):
			a.LastEvent = line
		case strings.Contains(line, "PR has been merged"):
			a.LastEvent = line
		case strings.Contains(line, "PR has been closed"):
			a.LastEvent = line
		case strings.Contains(line, "babysit timeout"):
			a.LastEvent = line
		}
	}
	return a
}

// renderBabysitView renders the babysit-specific monitoring view.
// Shown instead of generic findings when the babysit step is active.
func renderBabysitView(run *ipc.RunInfo, steps []ipc.StepResultInfo, findings string, logs []string, width int) string {
	return renderBabysitViewWithSelection(run, steps, findings, logs, width, 0, 0, nil)
}

func renderBabysitViewWithSelection(run *ipc.RunInfo, steps []ipc.StepResultInfo, findings string, logs []string, width int, height int, cursor int, selected map[string]bool) string {
	var b strings.Builder

	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	contentWidth := boxWidth - 4 // account for box border + padding

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// PR info - try run.PRURL first, then parse from logs.
	// Truncated to fit inside the box.
	if run != nil && run.PRURL != nil && *run.PRURL != "" {
		prText := "PR: " + *run.PRURL
		prText, _ = cutText(prText, contentWidth)
		b.WriteString(dimStyle.Render(prText) + "\n")
	} else if num := extractPRFromLogs(logs); num != "" {
		b.WriteString(dimStyle.Render("PR #"+num) + "\n")
	}

	// State indicator.
	status := babysitStepStatus(steps)
	activity := parseBabysitActivity(logs)

	b.WriteString("\n")

	switch status {
	case types.StepStatusRunning:
		if activity.AutoFixing {
			style := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlue))
			b.WriteString(style.Render("\u2699 Auto-fixing CI failures...") + "\n")
		} else {
			style := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
			b.WriteString(style.Render("◉ Monitoring CI and PR comments...") + "\n")
		}
	case types.StepStatusFixing:
		style := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlue))
		b.WriteString(style.Render("⚙ Agent addressing PR comments...") + "\n")
	case types.StepStatusAwaitingApproval, types.StepStatusFixReview:
		style := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))
		b.WriteString(style.Render("⏸ New PR comments - review below") + "\n")
	}

	// CI auto-fix count.
	if activity.CIFixes > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("CI auto-fixes: %d", activity.CIFixes)) + "\n")
	}

	// Last activity, truncated to fit inside the box.
	if activity.LastEvent != "" {
		eventText := "Latest: " + activity.LastEvent
		eventText, _ = cutText(eventText, contentWidth)
		b.WriteString(dimStyle.Render(eventText) + "\n")
	}

	// Log tail during monitoring (non-approval) states.
	// Adaptive line count: 5 for height >= 30, 3 for 20-29, hidden for < 20.
	isApproval := status == types.StepStatusAwaitingApproval || status == types.StepStatusFixReview
	logLines := 5
	if height > 0 && height < 30 {
		logLines = 3
	}
	if height > 0 && height < 20 {
		logLines = 0
	}

	if !isApproval && len(logs) > 0 && logLines > 0 {
		b.WriteString("\n")
		start := len(logs) - logLines
		if start < 0 {
			start = 0
		}
		for _, line := range logs[start:] {
			line, _ = cutText(line, contentWidth)
			b.WriteString(styleLogLine(line) + "\n")
		}
	}
	var itemCount int
	if isApproval && findings != "" {
		if f, err := parseFindings(findings); err == nil && f != nil {
			itemCount = len(f.Items)
		}
		// Compute viewport size from available height.
		// Reserve ~10 lines for babysit header content (PR info, state, activity, box border).
		maxVisible := 0
		if height > 0 {
			findingsHeight := height - 25 // reserve for pipeline, babysit header, box borders, footer
			if findingsHeight > 6 {
				maxVisible = findingsHeight / 3 // ~3 lines per finding
			}
		}
		rendered, scrollFooter := renderFindingsWithSelection(findings, contentWidth, cursor, selected, maxVisible)
		if rendered != "" {
			b.WriteString("\n")
			b.WriteString(rendered)
			// Babysit findings are nested inside the babysit box, so inline the scroll footer.
			if scrollFooter != "" {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
				b.WriteString("\n")
				b.WriteString(dimStyle.Render(scrollFooter))
				b.WriteString("\n")
			}
		}
	}

	title := "Babysit"
	if itemCount > 0 {
		title += fmt.Sprintf(" (%d/%d)", cursor+1, itemCount)
	}
	return renderBox(title, b.String(), boxWidth)
}

func renderBabysitApprovalViewForHeight(run *ipc.RunInfo, steps []ipc.StepResultInfo, findings string, logs []string, width int, boxHeight int, cursor int, selected map[string]bool) string {
	if boxHeight > 0 && boxHeight < 3 {
		return ""
	}
	if boxHeight <= 0 {
		return renderBabysitViewWithSelection(run, steps, findings, logs, width, 0, cursor, selected)
	}

	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	contentWidth := boxWidth - 4
	contentBudget := boxHeight - 2
	if contentBudget <= 0 {
		return ""
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	activity := parseBabysitActivity(logs)

	var lines []string
	if run != nil && run.PRURL != nil && *run.PRURL != "" {
		prText := "PR: " + *run.PRURL
		prText, _ = cutText(prText, contentWidth)
		lines = append(lines, dimStyle.Render(prText))
	} else if num := extractPRFromLogs(logs); num != "" {
		lines = append(lines, dimStyle.Render("PR #"+num))
	}
	lines = append(lines, "")
	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))
	lines = append(lines, statusStyle.Render("⏸ New PR comments - review below"))
	if activity.CIFixes > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("CI auto-fixes: %d", activity.CIFixes)))
	}
	if activity.LastEvent != "" {
		eventText := "Latest: " + activity.LastEvent
		eventText, _ = cutText(eventText, contentWidth)
		lines = append(lines, dimStyle.Render(eventText))
	}

	header := strings.Join(lines, "\n")
	remaining := contentBudget - lipgloss.Height(header)
	content := header

	itemCount := 0
	if f, err := parseFindings(findings); err == nil && f != nil {
		itemCount = len(f.Items)
	}
	if findings != "" && remaining > 1 {
		rendered, scrollFooter := renderFindingsWithSelectionHeight(findings, contentWidth, cursor, selected, remaining-1)
		if scrollFooter != "" && remaining > 2 {
			rendered, scrollFooter = renderFindingsWithSelectionHeight(findings, contentWidth, cursor, selected, remaining-2)
		}
		if rendered != "" {
			content += "\n" + rendered
			if scrollFooter != "" {
				content += "\n" + dimStyle.Render(scrollFooter)
			}
		}
	}

	title := "Babysit"
	if itemCount > 0 {
		title += fmt.Sprintf(" (%d/%d)", cursor+1, itemCount)
	}
	return renderBox(title, content, boxWidth)
}

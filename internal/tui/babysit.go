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
	return renderBabysitViewWithSelection(run, steps, findings, logs, width, 0, nil)
}

func renderBabysitViewWithSelection(run *ipc.RunInfo, steps []ipc.StepResultInfo, findings string, logs []string, width int, cursor int, selected map[string]bool) string {
	var b strings.Builder

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// PR info - try run.PRURL first, then parse from logs.
	if run != nil && run.PRURL != nil && *run.PRURL != "" {
		b.WriteString(dimStyle.Render("PR: "+*run.PRURL) + "\n")
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

	// Last activity.
	if activity.LastEvent != "" {
		b.WriteString(dimStyle.Render("Latest: "+activity.LastEvent) + "\n")
	}

	// Comment findings when awaiting approval.
	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	contentWidth := boxWidth - 4 // account for box border + padding
	if (status == types.StepStatusAwaitingApproval || status == types.StepStatusFixReview) && findings != "" {
		rendered := renderFindingsWithSelection(findings, contentWidth, cursor, selected, 0)
		if rendered != "" {
			b.WriteString("\n")
			b.WriteString(rendered)
		}
	}

	return renderBox("Babysit", b.String(), boxWidth)
}

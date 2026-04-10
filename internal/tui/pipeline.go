package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// stepStatusIcon returns the visual indicator for a step's status.
func stepStatusIcon(status types.StepStatus) string {
	return stepStatusIndicator(status, 0)
}

func stepStatusIndicator(status types.StepStatus, spinnerFrame int) string {
	switch status {
	case types.StepStatusPending:
		return "○"
	case types.StepStatusRunning, types.StepStatusFixing:
		if len(spinnerFrames) == 0 {
			return "◉"
		}
		if spinnerFrame < 0 {
			spinnerFrame = 0
		}
		return spinnerFrames[spinnerFrame%len(spinnerFrames)]
	case types.StepStatusAwaitingApproval:
		return "⏸"
	case types.StepStatusFixReview:
		return "⏸"
	case types.StepStatusCompleted:
		return "✓"
	case types.StepStatusSkipped:
		return "–"
	case types.StepStatusFailed:
		return "✗"
	default:
		return "?"
	}
}

// stepStatusStyle returns the lipgloss style for a step's status indicator.
func stepStatusStyle(status types.StepStatus) lipgloss.Style {
	switch status {
	case types.StepStatusRunning, types.StepStatusFixing:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlue))
	case types.StepStatusAwaitingApproval, types.StepStatusFixReview:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))
	case types.StepStatusCompleted:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	case types.StepStatusSkipped:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	case types.StepStatusFailed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	}
}

// stepLabel returns the human-readable label for a step name.
func stepLabel(name types.StepName) string {
	switch name {
	case types.StepReview:
		return "Review"
	case types.StepTest:
		return "Test"
	case types.StepLint:
		return "Lint"
	case types.StepPush:
		return "Push"
	case types.StepPR:
		return "PR"
	case types.StepBabysit:
		return "Babysit"
	default:
		return string(name)
	}
}

// formatDuration formats milliseconds into a human-readable duration.
func formatDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// renderPipelineView renders the step list with status indicators inside a boxed section.
// When height < 30, connector lines between steps are suppressed to save vertical space.
func renderPipelineView(run *ipc.RunInfo, steps []ipc.StepResultInfo, width int, spinnerFrame int, height int) string {
	if run == nil {
		return "No active run."
	}

	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	contentWidth := boxWidth - 4 // 2 border + 2 padding

	var b strings.Builder

	// Header, truncated to fit inside the box.
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	header := fmt.Sprintf("%s @ %s", run.Branch, run.HeadSHA[:min(8, len(run.HeadSHA))])
	header, _ = cutText(header, contentWidth)
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("%s  %s", run.ID, run.Status)))
	b.WriteString("\n")

	// Step list with connectors.
	for i, step := range steps {
		icon := stepStatusIndicator(step.Status, spinnerFrame)
		style := stepStatusStyle(step.Status)
		label := stepLabel(step.StepName)

		line := style.Render(icon) + " " + label

		// Add duration if completed.
		if step.DurationMS != nil {
			line += "  " + dimStyle.Render(formatDuration(*step.DurationMS))
		}

		// Add status suffix for non-obvious states (dim per Typography Scale "Meta").
		// Error messages are truncated to fit within the remaining line width.
		switch step.Status {
		case types.StepStatusAwaitingApproval:
			line += " " + dimStyle.Render("- awaiting approval")
		case types.StepStatusFixing:
			line += " " + dimStyle.Render("- agent fixing...")
		case types.StepStatusFixReview:
			line += " " + dimStyle.Render("- review fix")
		case types.StepStatusFailed:
			if step.Error != nil {
				errText := "- " + *step.Error
				remaining := contentWidth - lipgloss.Width(line) - 1 // -1 for space before suffix
				if remaining > 0 && lipgloss.Width(errText) > remaining {
					errText, _ = cutText(errText, remaining)
				}
				line += " " + dimStyle.Render(errText)
			}
		}

		b.WriteString(line)
		b.WriteString("\n")

		// Connector between steps (suppressed in compact mode for small terminals).
		if i < len(steps)-1 && height >= 30 {
			b.WriteString(dimStyle.Render("│") + "\n")
		}
	}

	// Run error, truncated to fit inside the box.
	if run.Error != nil {
		errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
		errText := "Error: " + *run.Error
		errText, _ = cutText(errText, contentWidth)
		b.WriteString("\n" + errStyle.Render(errText) + "\n")
	}
	return renderBox("Pipeline", b.String(), boxWidth)
}

// renderActionBar renders the approval prompt and action keys as a standalone element.
// Per DESIGN.md: "Sits below the pipeline box, above findings/diff"
// showDiff controls whether the 'd' key label says "findings" (to toggle back) or "diff".
// Selection actions are hidden in diff mode since they don't apply.
func renderActionBar(steps []ipc.StepResultInfo, showSelectionActions bool, allowFix bool, showDiff bool, selectedCount int, totalCount int, confirmAbort bool, hasDiff bool) string {
	step := awaitingStep(steps)
	if step == nil {
		return ""
	}

	var b strings.Builder
	promptStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiYellow))
	b.WriteString(promptStyle.Render(fmt.Sprintf("%s awaiting action:", stepLabel(step.StepName))))
	b.WriteString("\n")
	// Hide selection actions in diff mode since toggle/A/N keys don't work there.
	effectiveSelection := showSelectionActions && !showDiff
	b.WriteString(renderApprovalActions(effectiveSelection, allowFix, showDiff, selectedCount, totalCount, confirmAbort, hasDiff))
	return b.String()
}

func renderApprovalActions(showSelectionActions bool, allowFix bool, showDiff bool, selectedCount int, totalCount int, confirmAbort bool, hasDiff bool) string {
	boldKey := lipgloss.NewStyle().Bold(true)
	renderAction := func(key, label string) string {
		return boldKey.Render(key) + " " + label
	}

	primary := []string{renderAction("a", "approve")}
	if allowFix {
		fixLabel := "fix"
		if selectedCount > 0 && selectedCount < totalCount {
			fixLabel = fmt.Sprintf("fix (%d/%d)", selectedCount, totalCount)
		}
		primary = append(primary, renderAction("f", fixLabel))
	}
	abortLabel := "abort"
	if confirmAbort {
		warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
		abortLabel = warnStyle.Render("x again to abort")
	}
	primary = append(primary, renderAction("s", "skip"), renderAction("x", abortLabel))
	if hasDiff {
		diffLabel := "diff"
		if showDiff {
			diffLabel = "findings"
		}
		primary = append(primary, renderAction("d", diffLabel))
	}

	result := " " + strings.Join(primary, "  ")

	if showSelectionActions {
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
		selection := []string{renderAction("\u2423", "toggle"), renderAction("A", "all"), renderAction("N", "none")}
		result += " " + dimStyle.Render("│") + " " + strings.Join(selection, "  ")
	}

	return result + "\n"
}

// renderOutcomeBanner returns a styled one-line banner when the run is done.
// Empty string when the run is still in progress.
func renderOutcomeBanner(run *ipc.RunInfo, steps []ipc.StepResultInfo) string {
	if run == nil {
		return ""
	}

	// Sum step durations for elapsed time.
	elapsed := ""
	var totalMS int64
	for _, s := range steps {
		if s.DurationMS != nil {
			totalMS += *s.DurationMS
		}
	}
	if totalMS > 0 {
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
		elapsed = "  " + dimStyle.Render(formatDuration(totalMS))
	}

	switch run.Status {
	case types.RunCompleted:
		style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiGreen))
		return style.Render("✓ Pipeline passed") + elapsed
	case types.RunFailed:
		// Find which step failed.
		failedLabel := ""
		for _, s := range steps {
			if s.Status == types.StepStatusFailed {
				failedLabel = stepLabel(s.StepName)
				break
			}
		}
		style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiRed))
		if failedLabel != "" {
			return style.Render("✗ "+failedLabel+" failed") + elapsed
		}
		return style.Render("✗ Pipeline failed") + elapsed
	default:
		return ""
	}
}

// renderHelpOverlay renders a help box showing keybindings relevant to the current state.
func renderHelpOverlay(width int, hasAwaitingStep bool, showDiff bool) string {
	boldKey := lipgloss.NewStyle().Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	section := func(title string, entries []string) string {
		var b strings.Builder
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiCyan)).Render(title))
		b.WriteString("\n")
		for _, e := range entries {
			b.WriteString("  " + e + "\n")
		}
		return b.String()
	}

	entry := func(key, desc string) string {
		return boldKey.Render(key) + "  " + dimStyle.Render(desc)
	}

	var content strings.Builder

	content.WriteString(section("Navigation", []string{
		entry("j/k", "scroll line by line"),
		entry("g/G", "jump to start/end"),
		entry("Ctrl+d/u", "half-page down/up"),
	}))
	if hasAwaitingStep {
		content.WriteString("\n")
		content.WriteString(section("Actions", []string{
			entry("a", "approve"),
			entry("f", "fix"),
			entry("s", "skip"),
			entry("x x", "abort (press twice)"),
			entry("d", "diff/findings toggle"),
		}))
		if !showDiff {
			content.WriteString("\n")
			content.WriteString(section("Selection", []string{
				entry("\u2423", "toggle current"),
				entry("A", "select all"),
				entry("N", "select none"),
			}))
		}
	}
	content.WriteString("\n")
	content.WriteString(entry("q", "detach/quit") + "\n")
	content.WriteString(entry("?", "close help") + "\n")

	return renderBox("Help", content.String(), width)
}

// awaitingStep returns the step that is currently awaiting user action, if any.
func awaitingStep(steps []ipc.StepResultInfo) *ipc.StepResultInfo {
	for i := range steps {
		if steps[i].Status == types.StepStatusAwaitingApproval || steps[i].Status == types.StepStatusFixReview {
			return &steps[i]
		}
	}
	return nil
}

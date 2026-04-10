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
func renderPipelineView(run *ipc.RunInfo, steps []ipc.StepResultInfo, width int, spinnerFrame int, showSelectionActions bool, allowFix bool) string {
	if run == nil {
		return "No active run."
	}

	var b strings.Builder

	// Header.
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	b.WriteString(fmt.Sprintf("%s @ %s", run.Branch, run.HeadSHA[:min(8, len(run.HeadSHA))]))
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
		switch step.Status {
		case types.StepStatusAwaitingApproval:
			line += " " + dimStyle.Render("- awaiting approval")
		case types.StepStatusFixing:
			line += " " + dimStyle.Render("- agent fixing...")
		case types.StepStatusFixReview:
			line += " " + dimStyle.Render("- review fix")
		case types.StepStatusFailed:
			if step.Error != nil {
				line += " " + dimStyle.Render("- "+*step.Error)
			}
		}

		b.WriteString(line)
		b.WriteString("\n")

		// Connector between steps.
		if i < len(steps)-1 {
			b.WriteString(dimStyle.Render("│") + "\n")
		}
	}

	// Approval prompt if any step is awaiting.
	for _, step := range steps {
		if step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview {
			b.WriteString("\n")
			promptStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiYellow))
			b.WriteString(promptStyle.Render(fmt.Sprintf("%s awaiting action:", stepLabel(step.StepName))))
			b.WriteString("\n")
			b.WriteString(renderApprovalActions(showSelectionActions, allowFix))
			break
		}
	}

	// Run error.
	if run.Error != nil {
		errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
		b.WriteString("\n" + errStyle.Render("Error: "+*run.Error) + "\n")
	}

	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}
	return renderBox("Pipeline", b.String(), boxWidth)
}

func renderApprovalActions(showSelectionActions bool, allowFix bool) string {
	boldKey := lipgloss.NewStyle().Bold(true)
	renderAction := func(key, label string) string {
		return boldKey.Render(key) + " " + label
	}

	primary := []string{renderAction("a", "approve")}
	if allowFix {
		primary = append(primary, renderAction("f", "fix"))
	}
	primary = append(primary, renderAction("s", "skip"), renderAction("x", "abort"), renderAction("d", "diff"))

	result := " " + strings.Join(primary, "  ")

	if showSelectionActions {
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
		selection := []string{renderAction("\u2423", "toggle"), renderAction("A", "all"), renderAction("N", "none")}
		result += " " + dimStyle.Render("│") + " " + strings.Join(selection, "  ")
	}

	return result + "\n"
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

package wizard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ANSI-only palette, matching internal/tui/theme.go.
const (
	ansiRed         = "1"
	ansiGreen       = "2"
	ansiYellow      = "3"
	ansiBlue        = "4"
	ansiCyan        = "6"
	ansiBrightBlack = "8"
)

func init() {
	lipgloss.SetColorProfile(termenv.ANSI)
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// View renders the wizard.
func (m Model) View() string {
	width := m.width
	if width < 60 {
		width = 60
	}
	m.width = width

	var content strings.Builder

	for i, s := range m.steps {
		content.WriteString(m.renderStep(s))
		content.WriteString("\n")
		if i < len(m.steps)-1 {
			content.WriteString(dimStyle().Render("│"))
			content.WriteString("\n")
		}
	}

	box := renderBox("Setup", strings.TrimRight(content.String(), "\n"), width)

	var out strings.Builder
	out.WriteString(box)
	out.WriteString("\n")

	if bar := m.renderActionBar(); bar != "" {
		out.WriteString("\n")
		out.WriteString(bar)
		out.WriteString("\n")
	}

	out.WriteString("\n")
	out.WriteString(m.renderFooter())
	out.WriteString("\n")

	if m.err != nil {
		out.WriteString("\n")
		out.WriteString(redStyle().Render("error: " + m.err.Error()))
		out.WriteString("\n")
	}

	return out.String()
}

func (m Model) renderStep(s *step) string {
	icon, style := stepIconAndStyle(s, m.spinnerFrame)
	header := style.Render(icon) + " " + stepLabel(s.id)

	switch s.status {
	case statDone:
		return header + "  " + dimStyle().Render(s.result)

	case statSkipped:
		if s.skipReason != "" {
			return header + "  " + dimStyle().Render(s.skipReason)
		}
		return header

	case statFailed:
		lines := []string{header + "  " + redStyle().Render(s.errMsg)}
		return strings.Join(lines, "\n")

	case statInput:
		return header + "  " + m.inlineInputView(header)

	case statAgent:
		return header + "  " + dimStyle().Render(agentLabel(s.id))

	case statRunning:
		return header + "  " + dimStyle().Render(runningLabel(s.id, s.result))

	case statConfirm:
		return header + "  " + dimStyle().Render(confirmLabel(m.cfg.GateRemote, m.targetBranch))
	}
	return header
}

func (m Model) inlineInputView(header string) string {
	input := m.input
	contentWidth := m.width - 4
	availableWidth := contentWidth - lipgloss.Width(header) - 2
	promptWidth := lipgloss.Width(input.Prompt)
	fieldWidth := availableWidth - promptWidth - 1
	if fieldWidth < 1 {
		fieldWidth = 1
	}
	input.Width = fieldWidth
	input.SetCursor(input.Position())
	return input.View()
}

func (m Model) renderActionBar() string {
	s := m.activeStep()
	if s == nil {
		return ""
	}
	switch s.status {
	case statInput:
		return " " + boldStyle().Render("⏎") + " submit"
	case statConfirm:
		return " " + boldStyle().Render("y") + " push  " + boldStyle().Render("n") + " skip"
	case statFailed:
		return " " + boldStyle().Render("r") + " retry"
	}
	return ""
}

func (m Model) renderFooter() string {
	if m.confirmQuit {
		msg := " " + boldStyle().Render("q") + " again to abort  " + dimStyle().Render(m.sideEffectsDescription()+" will stay")
		return redStyle().Render(msg)
	}
	if m.hasSideEffects() {
		return " " + boldStyle().Render("q") + " abort  " + dimStyle().Render(m.sideEffectsDescription()+" stays")
	}
	return " " + boldStyle().Render("q") + " quit"
}

func (m Model) sideEffectsDescription() string {
	parts := []string{}
	if m.branchCreated {
		parts = append(parts, "branch "+m.targetBranch)
	}
	if m.commitMade {
		parts = append(parts, "commit")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " + ")
}

func stepLabel(id stepID) string {
	switch id {
	case stepBranch:
		return "Branch"
	case stepCommit:
		return "Commit"
	case stepPush:
		return "Push"
	}
	return "?"
}

func inputLabel(id stepID) string {
	switch id {
	case stepBranch:
		return "branch name (blank to let the agent suggest):"
	case stepCommit:
		return "commit message (blank to let the agent suggest):"
	}
	return ""
}

func agentLabel(id stepID) string {
	switch id {
	case stepBranch:
		return "asking agent for a branch name…"
	case stepCommit:
		return "asking agent for a commit message…"
	}
	return "asking agent…"
}

func runningLabel(id stepID, value string) string {
	switch id {
	case stepBranch:
		return "creating branch " + value + "…"
	case stepCommit:
		return "committing \"" + value + "\"…"
	case stepPush:
		return "pushing to gate…"
	}
	return "working…"
}

func confirmLabel(remote, branch string) string {
	if remote == "" {
		remote = "no-mistakes"
	}
	return fmt.Sprintf("push %s to %s gate?", branch, remote)
}

func stepIconAndStyle(s *step, spinnerFrame int) (string, lipgloss.Style) {
	switch s.status {
	case statDone:
		return "✓", greenStyle()
	case statSkipped:
		return "–", dimStyle()
	case statFailed:
		return "✗", redStyle()
	case statAgent, statRunning:
		if len(spinnerFrames) == 0 {
			return "◉", blueStyle()
		}
		return spinnerFrames[spinnerFrame%len(spinnerFrames)], blueStyle()
	case statInput:
		return "⏸", yellowStyle()
	case statConfirm:
		return "⏸", yellowStyle()
	}
	return "○", dimStyle()
}

// Style helpers — kept as functions so tests can swap the color profile.

func dimStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
}
func greenStyle() lipgloss.Style  { return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen)) }
func redStyle() lipgloss.Style    { return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed)) }
func yellowStyle() lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow)) }
func blueStyle() lipgloss.Style   { return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlue)) }
func cyanStyle() lipgloss.Style   { return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiCyan)) }
func boldStyle() lipgloss.Style   { return lipgloss.NewStyle().Bold(true) }

// renderBox mirrors internal/tui/box.go: a rounded-border box with a cyan
// bold title embedded in the top border.
func renderBox(title, content string, width int) string {
	if width < 6 {
		width = 6
	}
	titleStyled := cyanStyle().Bold(true).Render(title)
	borderColor := dimStyle()

	titleWidth := lipgloss.Width(titleStyled)
	fillWidth := width - 5 - titleWidth
	if fillWidth < 1 {
		fillWidth = 1
	}
	topBorder := borderColor.Render("╭─ ") + titleStyled + " " + borderColor.Render(strings.Repeat("─", fillWidth)+"╮")

	contentWidth := width - 4
	if contentWidth < 1 {
		contentWidth = 1
	}

	var lines []string
	for _, cl := range strings.Split(content, "\n") {
		visWidth := lipgloss.Width(cl)
		pad := contentWidth - visWidth
		if pad < 0 {
			pad = 0
		}
		line := borderColor.Render("│") + " " + cl + strings.Repeat(" ", pad) + " " + borderColor.Render("│")
		lines = append(lines, line)
	}

	fill := width - 2
	if fill < 1 {
		fill = 1
	}
	bottom := borderColor.Render("╰" + strings.Repeat("─", fill) + "╯")

	return topBorder + "\n" + strings.Join(lines, "\n") + "\n" + bottom
}

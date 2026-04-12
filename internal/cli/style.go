package cli

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var (
	sRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	sGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	sYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	sBlue   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	sCyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	sDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sBold   = lipgloss.NewStyle().Bold(true)
)

// runStatusStyle returns a styled string for the given run status.
func runStatusStyle(status types.RunStatus) string {
	s := string(status)
	switch status {
	case types.RunCompleted:
		return sGreen.Render(s)
	case types.RunFailed:
		return sRed.Render(s)
	case types.RunRunning:
		return sBlue.Render(s)
	default:
		return sDim.Render(s)
	}
}

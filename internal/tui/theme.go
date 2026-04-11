package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

const (
	ansiRed         = "1"
	ansiGreen       = "2"
	ansiYellow      = "3"
	ansiBlue        = "4"
	ansiCyan        = "6"
	ansiBrightBlack = "8"
)

func init() {
	configureTUIColors()
}

// configureTUIColors forces ANSI-profile colors so styling follows the user's
// terminal theme instead of hard-coded 256-color palette values.
func configureTUIColors() {
	lipgloss.SetColorProfile(termenv.ANSI)
}

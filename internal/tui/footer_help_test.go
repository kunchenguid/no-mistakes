package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestModel_View_FooterShowsCloseWhenHelpVisible(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// Footer should say "close" instead of "help" when help overlay is visible.
	lines := strings.Split(plain, "\n")
	for _, line := range lines {
		// Look for footer line (outside the help box) that has the ? key hint.
		// The footer is the last non-empty line.
		if strings.Contains(line, "?") && strings.Contains(line, "close") && !strings.Contains(line, "close help") {
			return // found the footer with "close" label
		}
	}
	t.Errorf("footer should show '? close' when help is visible, got:\n%s", plain)
}

func TestModel_View_FooterShowsHelpWhenHelpHidden(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = false

	view := m.View()
	plain := stripANSI(view)

	// Footer should say "help" when help overlay is NOT visible.
	lines := strings.Split(plain, "\n")
	for _, line := range lines {
		if strings.Contains(line, "?") && strings.Contains(line, "help") {
			return // found the footer with "help" label
		}
	}
	t.Errorf("footer should show '? help' when help is hidden, got:\n%s", plain)
}

func TestModel_View_FooterNeverShowsHelpWhenHelpVisible(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// The footer line (last few lines, outside boxes) should NOT say "help"
	// when help overlay is already showing. It should say "close" instead.
	// Find footer lines (after the last box closing border ╰).
	lines := strings.Split(plain, "\n")
	// The footer is the last non-empty line(s) after all boxes.
	lastBoxEnd := 0
	for i, line := range lines {
		if strings.Contains(line, "╰") || strings.Contains(line, "+") {
			lastBoxEnd = i
		}
	}
	for _, line := range lines[lastBoxEnd+1:] {
		if strings.Contains(line, "?") && strings.Contains(line, "help") && !strings.Contains(line, "close") {
			t.Errorf("footer should NOT show '? help' when help is visible, found: %q", line)
		}
	}
}

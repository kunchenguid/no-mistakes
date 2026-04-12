package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderBox renders content inside a rounded-border box with a styled title
// embedded in the top border, per DESIGN.md Boxed Sections spec.
func renderBox(title, content string, width int) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiCyan))
	return renderBoxWithStyledTitle(titleStyle.Render(title), content, width, "")
}

// renderBoxWithFooter renders a box with an optional hint embedded in the bottom border.
// Per DESIGN.md Diff View: ╰──── ↓ 23 more lines (j/k) ─────────────╯
func renderBoxWithFooter(title, content string, width int, footer string) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiCyan))
	return renderBoxWithStyledTitle(titleStyle.Render(title), content, width, footer)
}

// renderBoxWithStyledTitle renders a box with a pre-styled title and optional footer.
// Unlike renderBoxWithFooter, the title is used as-is (caller handles styling).
func renderBoxWithStyledTitle(styledTitle string, content string, width int, footer string) string {
	if width < 6 {
		width = 6
	}

	borderColor := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Top border.
	titleWidth := lipgloss.Width(styledTitle)
	fillWidth := width - 5 - titleWidth
	if fillWidth < 1 {
		fillWidth = 1
	}
	topBorder := borderColor.Render("╭─ ") + styledTitle + " " + borderColor.Render(strings.Repeat("─", fillWidth)+"╮")

	// Content width.
	contentWidth := width - 4
	if contentWidth < 1 {
		contentWidth = 1
	}

	// Content lines.
	var lines []string
	contentLines := strings.Split(content, "\n")
	if len(contentLines) > 0 && contentLines[len(contentLines)-1] == "" {
		contentLines = contentLines[:len(contentLines)-1]
	}
	for _, cl := range contentLines {
		visWidth := lipgloss.Width(cl)
		pad := contentWidth - visWidth
		if pad < 0 {
			pad = 0
		}
		line := borderColor.Render("│") + " " + cl + strings.Repeat(" ", pad) + " " + borderColor.Render("│")
		lines = append(lines, line)
	}

	return topBorder + "\n" + strings.Join(lines, "\n") + "\n" + renderBottomBorder(width, footer)
}

// renderBottomBorder renders a bottom border, optionally embedding a footer hint.
func renderBottomBorder(width int, footer string) string {
	borderColor := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	if footer == "" {
		fill := width - 2
		if fill < 1 {
			fill = 1
		}
		return borderColor.Render("╰" + strings.Repeat("─", fill) + "╯")
	}

	// ╰──── footer ─────╯
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	leadDashes := 4
	maxFooterWidth := width - leadDashes - 5
	if maxFooterWidth < 1 {
		maxFooterWidth = 1
	}
	footer, _ = cutText(footer, maxFooterWidth)
	footerRendered := dimStyle.Render(footer)
	footerWidth := lipgloss.Width(footerRendered)
	trailingFill := width - footerWidth - 8
	if trailingFill < 1 {
		trailingFill = 1
	}
	return borderColor.Render("╰"+strings.Repeat("─", leadDashes)+" ") + footerRendered + " " + borderColor.Render(strings.Repeat("─", trailingFill)+"╯")
}

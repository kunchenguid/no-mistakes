package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	nmtypes "github.com/kunchenguid/no-mistakes/internal/types"
)

// finding mirrors pipeline/steps.Finding for TUI rendering.
type finding = nmtypes.Finding

// findings mirrors pipeline/steps.Findings for TUI rendering.
type findings = nmtypes.Findings

// parseFindings decodes a JSON-encoded findings string.
func parseFindings(raw string) (*findings, error) {
	if raw == "" {
		return nil, nil
	}
	f, err := nmtypes.ParseFindingsJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse findings: %w", err)
	}
	return &f, nil
}

// severityIcon returns the visual indicator for a finding severity.
func severityIcon(severity string) string {
	switch severity {
	case "error":
		return "●"
	case "warning":
		return "▲"
	case "info":
		return "○"
	default:
		return "·"
	}
}

// severityStyle returns the lipgloss style for a finding severity.
func severityStyle(severity string) lipgloss.Style {
	switch severity {
	case "error":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	case "warning":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))
	case "info":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlue))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	}
}

func wrapText(text string, width int) []string {
	if width <= 0 || text == "" {
		return []string{text}
	}

	paragraphs := strings.Split(text, "\n")
	lines := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}

		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}

		current := ""
		for _, word := range words {
			for lipgloss.Width(word) > width {
				part, rest := cutText(word, width)
				if current != "" {
					lines = append(lines, current)
					current = ""
				}
				lines = append(lines, part)
				word = rest
			}

			if current == "" {
				current = word
				continue
			}
			if lipgloss.Width(current)+1+lipgloss.Width(word) <= width {
				current += " " + word
				continue
			}
			lines = append(lines, current)
			current = word
		}
		if current != "" {
			lines = append(lines, current)
		}
	}
	return lines
}

func cutText(text string, width int) (string, string) {
	if width <= 0 || text == "" {
		return text, ""
	}

	var b strings.Builder
	currentWidth := 0
	for i, r := range text {
		runeWidth := lipgloss.Width(string(r))
		if currentWidth+runeWidth > width && b.Len() > 0 {
			return b.String(), text[i:]
		}
		b.WriteRune(r)
		currentWidth += runeWidth
	}
	return b.String(), ""
}

func wrapIndentedText(text string, width, indent int) string {
	if text == "" {
		return strings.Repeat(" ", indent)
	}

	wrapWidth := 0
	if width > indent {
		wrapWidth = width - indent
	}

	prefix := strings.Repeat(" ", indent)
	wrapped := wrapText(text, wrapWidth)
	var b strings.Builder
	for i, line := range wrapped {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(prefix)
		b.WriteString(line)
	}
	return b.String()
}

// renderFindings renders a findings list for display in the TUI.
func renderFindings(raw string, width int) string {
	f, err := parseFindings(raw)
	if err != nil || f == nil {
		return ""
	}
	selected := make(map[string]bool, len(f.Items))
	for _, item := range f.Items {
		if item.ID != "" {
			selected[item.ID] = true
		}
	}
	content, _ := renderFindingsWithSelection(raw, width, 0, selected, 0)
	return content
}

// renderFindingsWithSelection renders findings with cursor, selection state, and optional
// viewport limiting. maxVisible <= 0 means show all items (no viewport).
// Returns (content, scrollFooter) where scrollFooter is a hint for the bottom border
// (non-empty when items exist below the viewport).
func renderFindingsWithSelection(raw string, width int, cursor int, selected map[string]bool, maxVisible int) (string, string) {
	f, err := parseFindings(raw)
	if err != nil || f == nil {
		return "", ""
	}

	if len(f.Items) == 0 && f.Summary == "" {
		return "", ""
	}

	var b strings.Builder

	// Summary line.
	if f.Summary != "" {
		summaryStyle := lipgloss.NewStyle().Bold(true)
		b.WriteString(summaryStyle.Render(wrapIndentedText(f.Summary, width, 0)))
		b.WriteString("\n")
	}

	// Count by severity.
	counts := map[string]int{}
	for _, item := range f.Items {
		counts[item.Severity]++
	}
	if len(counts) > 0 {
		var parts []string
		for _, sev := range []string{"error", "warning", "info"} {
			if c, ok := counts[sev]; ok {
				style := severityStyle(sev)
				parts = append(parts, style.Render(fmt.Sprintf("%d %s", c, sev)))
			}
		}
		b.WriteString(strings.Join(parts, "  "))
		b.WriteString("\n\n")
	}

	// Compute visible window of items.
	start := 0
	end := len(f.Items)
	if maxVisible > 0 && len(f.Items) > maxVisible {
		// Center the window on the cursor.
		start = cursor - maxVisible/2
		if start < 0 {
			start = 0
		}
		end = start + maxVisible
		if end > len(f.Items) {
			end = len(f.Items)
			start = end - maxVisible
			if start < 0 {
				start = 0
			}
		}
	}

	// Scroll-up indicator.
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	if start > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("↑ %d above (j/k)", start)))
		b.WriteString("\n\n")
	}

	// Individual findings.
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	blueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlue))
	for idx := start; idx < end; idx++ {
		item := f.Items[idx]
		// Blank line between findings per DESIGN.md Gutter System.
		if idx > start {
			b.WriteString("\n")
		}

		icon := severityIcon(item.Severity)
		style := severityStyle(item.Severity)
		// Checkbox: green when selected, dim when not (per DESIGN.md Color Roles).
		checkbox := dimStyle.Render("[ ]")
		if selected == nil || selected[item.ID] {
			checkbox = greenStyle.Render("[x]")
		}
		// Cursor: blue per DESIGN.md "Primary action/focus" for interactive elements.
		pointer := " "
		if idx == cursor {
			pointer = blueStyle.Render(">")
		}

		line := pointer + " " + checkbox + " " + style.Render(icon)

		// File:line reference.
		if item.File != "" {
			ref := item.File
			if item.Line > 0 {
				ref = fmt.Sprintf("%s:%d", item.File, item.Line)
			}
			line += " " + dimStyle.Render(ref)
		}

		b.WriteString(line + "\n")

		// Description indented.
		// Gutter width: cursor(1) + sp(1) + checkbox(3) + sp(1) + icon(1) + sp(1) = 8
		b.WriteString(wrapIndentedText(item.Description, width, 8) + "\n")
	}

	// Scroll-down footer for the box border.
	scrollFooter := ""
	remaining := len(f.Items) - end
	if remaining > 0 {
		scrollFooter = fmt.Sprintf("↓ %d more below (j/k)", remaining)
	}

	return b.String(), scrollFooter
}

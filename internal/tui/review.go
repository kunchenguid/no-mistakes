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
		return "E"
	case "warning":
		return "W"
	case "info":
		return "I"
	default:
		return "·"
	}
}

// riskLevelStyle returns the lipgloss style for a risk level.
func riskLevelStyle(level string) lipgloss.Style {
	switch strings.ToLower(level) {
	case "low":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	case "medium":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))
	case "high":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
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

func wrapLine(text string, width int) []string {
	if width <= 0 || text == "" {
		return []string{text}
	}

	parts := []string{}
	for text != "" {
		part, rest := cutText(text, width)
		parts = append(parts, part)
		text = rest
	}
	return parts
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

func logLineStyle(line string) lipgloss.Style {
	switch {
	case strings.HasPrefix(line, "PASS"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	case strings.HasPrefix(line, "FAIL"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	}
}

// styleLogLine applies PASS/FAIL-aware coloring to a single log line.
// PASS lines are green, FAIL lines are red, everything else is dim.
func styleLogLine(line string) string {
	return logLineStyle(line).Render(line)
}

func renderLogTail(logs []string, width int, maxLines int) []string {
	if len(logs) == 0 || width <= 0 || maxLines <= 0 {
		return nil
	}

	lines := make([]string, 0, len(logs))
	for _, line := range logs {
		style := logLineStyle(line)
		for _, part := range wrapLine(line, width) {
			lines = append(lines, style.Render(part))
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
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

func renderFindingsRange(f *findings, width int, cursor int, selected map[string]bool, start int, end int) (string, string) {
	if f == nil {
		return "", ""
	}
	if len(f.Items) == 0 && f.Summary == "" && f.RiskLevel == "" {
		return "", ""
	}
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(f.Items) {
		end = len(f.Items)
	}

	var b strings.Builder

	// Risk assessment (review step) or summary fallback (lint/test steps).
	if f.RiskLevel != "" {
		boldStyle := lipgloss.NewStyle().Bold(true)
		rStyle := riskLevelStyle(f.RiskLevel)
		prefix := boldStyle.Render("Risk:") + " " + rStyle.Render(strings.ToUpper(f.RiskLevel))
		if f.RiskRationale != "" {
			dashSep := " - "
			prefixWidth := lipgloss.Width(prefix) + len(dashSep)
			rationale := wrapIndentedText(f.RiskRationale, width, prefixWidth)
			rationale = strings.TrimLeft(rationale, " ")
			b.WriteString(prefix + dashSep + rationale)
		} else {
			b.WriteString(prefix)
		}
		b.WriteString("\n")
	} else if f.Summary != "" {
		summaryStyle := lipgloss.NewStyle().Bold(true)
		b.WriteString(summaryStyle.Render(wrapIndentedText(f.Summary, width, 0)))
		b.WriteString("\n")
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

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

		// Unfocused severity icons are dim to match description/file ref dimming.
		iconStyled := style.Render(icon)
		if idx != cursor {
			iconStyled = dimStyle.Render(icon)
		}
		line := pointer + " " + checkbox + " " + iconStyled

		// Tag user-authored findings right after the icon so they remain
		// visible when unfocused and do not shift alignment of the ref.
		if item.Source == nmtypes.FindingSourceUser {
			line += " " + blueStyle.Render("[user]")
		}

		// File:line reference, truncated to fit within content width.
		if item.File != "" {
			ref := item.File
			if item.Line > 0 {
				ref = fmt.Sprintf("%s:%d", item.File, item.Line)
			}
			// Gutter prefix: cursor(1) + sp(1) + checkbox(3) + sp(1) + icon(1) + sp(1) = 8
			maxRefWidth := width - 8 - 1 // -1 for space before ref
			if maxRefWidth > 0 && lipgloss.Width(ref) > maxRefWidth {
				ref, _ = cutText(ref, maxRefWidth)
			}
			if idx == cursor {
				line += " " + ref
			} else {
				line += " " + dimStyle.Render(ref)
			}
		}

		b.WriteString(line + "\n")

		// Description indented. Unfocused descriptions are dim to create contrast.
		// Gutter width: cursor(1) + sp(1) + checkbox(3) + sp(1) + icon(1) + sp(1) = 8
		desc := wrapIndentedText(item.Description, width, 8)
		if idx != cursor {
			desc = dimStyle.Render(desc)
		}
		b.WriteString(desc + "\n")

		if item.UserInstructions != "" {
			instr := wrapIndentedText("> "+item.UserInstructions, width, 8)
			b.WriteString(blueStyle.Render(instr) + "\n")
		}
	}

	// Scroll footer for the box border - combines up and down indicators.
	scrollFooter := ""
	remaining := len(f.Items) - end
	if start > 0 && remaining > 0 {
		scrollFooter = fmt.Sprintf("↑ %d above  ↓ %d more below (j/k)", start, remaining)
	} else if remaining > 0 {
		scrollFooter = fmt.Sprintf("↓ %d more below (j/k)", remaining)
	} else if start > 0 {
		scrollFooter = fmt.Sprintf("↑ %d above (j/k)", start)
	} else if len(f.Items) > 1 {
		scrollFooter = "(j/k)"
	}

	// Selection count by severity when not all findings are selected.
	if selected != nil && len(f.Items) > 0 {
		selCounts := map[string]int{}
		for _, item := range f.Items {
			if selected[item.ID] {
				selCounts[item.Severity]++
			}
		}
		totalSelected := 0
		for _, c := range selCounts {
			totalSelected += c
		}
		if totalSelected < len(f.Items) {
			var selParts []string
			for _, sev := range []string{"error", "warning", "info"} {
				if c, ok := selCounts[sev]; ok && c > 0 {
					selParts = append(selParts, severityStyle(sev).Render(fmt.Sprintf("%s %d", severityIcon(sev), c)))
				}
			}
			if len(selParts) > 0 {
				selText := strings.Join(selParts, " ") + " selected"
				if scrollFooter != "" {
					scrollFooter += "  ·  " + selText
				} else {
					scrollFooter = selText
				}
			}
		}
	}

	return b.String(), scrollFooter
}

func trimRenderedLines(s string, maxLines int) string {
	if maxLines <= 0 || s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n")
}

func renderFindingsWithSelectionHeight(raw string, width int, cursor int, selected map[string]bool, maxLines int) (string, string) {
	f, err := parseFindings(raw)
	if err != nil || f == nil {
		return "", ""
	}
	return renderParsedFindingsHeight(f, width, cursor, selected, maxLines)
}

func renderParsedFindingsHeight(f *findings, width int, cursor int, selected map[string]bool, maxLines int) (string, string) {
	if f == nil {
		return "", ""
	}
	if len(f.Items) == 0 && f.Summary == "" && f.RiskLevel == "" {
		return "", ""
	}
	if maxLines <= 0 {
		return "", ""
	}

	if len(f.Items) == 0 {
		content, footer := renderFindingsRange(f, width, 0, selected, 0, 0)
		return trimRenderedLines(content, maxLines), footer
	}

	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(f.Items) {
		cursor = len(f.Items) - 1
	}

	fullContent, fullFooter := renderFindingsRange(f, width, cursor, selected, 0, len(f.Items))
	if lipgloss.Height(fullContent) <= maxLines {
		return fullContent, fullFooter
	}

	for visible := len(f.Items); visible >= 1; visible-- {
		start := cursor - visible/2
		if start < 0 {
			start = 0
		}
		end := start + visible
		if end > len(f.Items) {
			end = len(f.Items)
			start = end - visible
			if start < 0 {
				start = 0
			}
		}
		content, footer := renderFindingsRange(f, width, cursor, selected, start, end)
		if lipgloss.Height(content) <= maxLines {
			return content, footer
		}
	}

	content, footer := renderFindingsRange(f, width, cursor, selected, cursor, cursor+1)
	return trimRenderedLines(content, maxLines), footer
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
	return renderParsedFindingsViewport(f, width, cursor, selected, maxVisible)
}

func renderParsedFindingsViewport(f *findings, width int, cursor int, selected map[string]bool, maxVisible int) (string, string) {
	if f == nil {
		return "", ""
	}
	if len(f.Items) == 0 && f.Summary == "" && f.RiskLevel == "" {
		return "", ""
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

	return renderFindingsRange(f, width, cursor, selected, start, end)
}

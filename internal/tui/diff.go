package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// diffLineType classifies a line in a unified diff.
type diffLineType int

const (
	diffLineContext    diffLineType = iota // unchanged context line
	diffLineAddition                       // added line (+)
	diffLineDeletion                       // deleted line (-)
	diffLineFileHeader                     // file header (diff --git, ---, +++)
	diffLineHunkHeader                     // hunk header (@@...@@)
)

// diffLine is a single classified line from a unified diff.
type diffLine struct {
	Type diffLineType
	Text string
}

// parseDiffLines parses a raw unified diff string into classified lines.
func parseDiffLines(raw string) []diffLine {
	if raw == "" {
		return nil
	}

	lines := strings.Split(raw, "\n")
	// Remove trailing empty line from split.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}

	result := make([]diffLine, 0, len(lines))
	for _, line := range lines {
		result = append(result, diffLine{
			Type: classifyDiffLine(line),
			Text: line,
		})
	}
	return result
}

// classifyDiffLine determines the type of a diff line from its prefix.
func classifyDiffLine(line string) diffLineType {
	switch {
	case strings.HasPrefix(line, "diff --git"):
		return diffLineFileHeader
	case strings.HasPrefix(line, "--- "):
		return diffLineFileHeader
	case strings.HasPrefix(line, "+++ "):
		return diffLineFileHeader
	case strings.HasPrefix(line, "index "):
		return diffLineFileHeader
	case strings.HasPrefix(line, "@@"):
		return diffLineHunkHeader
	case strings.HasPrefix(line, "+"):
		return diffLineAddition
	case strings.HasPrefix(line, "-"):
		return diffLineDeletion
	default:
		return diffLineContext
	}
}

// diffStats returns summary statistics from a parsed diff.
func diffStats(lines []diffLine) (files, additions, deletions int) {
	seen := map[string]bool{}
	for _, l := range lines {
		switch l.Type {
		case diffLineFileHeader:
			if strings.HasPrefix(l.Text, "+++ ") {
				name := strings.TrimPrefix(l.Text, "+++ ")
				if name != "/dev/null" {
					seen[name] = true
				}
			}
		case diffLineAddition:
			additions++
		case diffLineDeletion:
			deletions++
		}
	}
	files = len(seen)
	return
}

// diffLineStyle returns the lipgloss style for a diff line type.
func diffLineStyle(t diffLineType) lipgloss.Style {
	switch t {
	case diffLineAddition:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	case diffLineDeletion:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	case diffLineHunkHeader:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ansiCyan))
	case diffLineFileHeader:
		return lipgloss.NewStyle().Bold(true)
	default:
		return lipgloss.NewStyle()
	}
}

// renderDiff renders a scrollable, color-coded diff view inside a boxed section.
// offset is the scroll position (first visible line), viewHeight is the number of visible lines.
// If viewHeight <= 0, all lines are rendered. stepLabel is included in the box title when non-empty.
func renderDiff(raw string, width, viewHeight, offset int, stepLabel string) string {
	lines := parseDiffLines(raw)
	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder

	// Stats header.
	files, adds, dels := diffStats(lines)
	statsStyle := lipgloss.NewStyle().Bold(true)
	addStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	delStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	fileWord := "files"
	if files == 1 {
		fileWord = "file"
	}
	b.WriteString(statsStyle.Render(fmt.Sprintf("%d %s", files, fileWord)))
	b.WriteString("  ")
	b.WriteString(addStyle.Render(fmt.Sprintf("+%d", adds)))
	b.WriteString("  ")
	b.WriteString(delStyle.Render(fmt.Sprintf("-%d", dels)))
	b.WriteString("\n")

	// Clamp offset.
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		offset = len(lines) - 1
	}

	// Determine visible range.
	end := len(lines)
	if viewHeight > 0 {
		end = offset + viewHeight
		if end > len(lines) {
			end = len(lines)
		}
	} else {
		offset = 0
	}

	// Render visible lines.
	for _, dl := range lines[offset:end] {
		style := diffLineStyle(dl.Type)
		b.WriteString(style.Render(dl.Text))
		b.WriteString("\n")
	}

	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
	}

	// Build scroll hint for the bottom border.
	scrollHint := ""
	if viewHeight > 0 && len(lines) > viewHeight {
		remaining := len(lines) - end
		if offset > 0 && remaining > 0 {
			scrollHint = fmt.Sprintf("↑ %d  ↓ %d more lines (j/k)", offset, remaining)
		} else if remaining > 0 {
			scrollHint = fmt.Sprintf("↓ %d more lines (j/k)", remaining)
		} else if offset > 0 {
			scrollHint = fmt.Sprintf("↑ %d lines (j/k)", offset)
		}
	}

	title := "Diff"
	if stepLabel != "" {
		title = "Diff - " + stepLabel
	}

	return renderBoxWithFooter(title, b.String(), boxWidth, scrollHint)
}

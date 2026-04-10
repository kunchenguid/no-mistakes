package tui

import (
	"fmt"
	"strconv"
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

// findDiffOffset returns the best line index in parsed diff lines to scroll to
// for a given file path and line number. Returns 0 if not found.
func findDiffOffset(lines []diffLine, file string, line int) int {
	// Find the file's "diff --git" header and its hunks.
	fileHeaderIdx := -1
	for i, dl := range lines {
		if dl.Type == diffLineFileHeader && strings.HasPrefix(dl.Text, "diff --git") {
			// Check if this file header matches the target file.
			// Format: "diff --git a/path b/path"
			if strings.Contains(dl.Text, "/"+file) || strings.HasSuffix(dl.Text, " "+file) {
				fileHeaderIdx = i
			} else if fileHeaderIdx >= 0 {
				// We've passed the target file's section.
				break
			}
		}
		if fileHeaderIdx >= 0 && dl.Type == diffLineHunkHeader && line > 0 {
			// Parse hunk header: @@ -oldStart,oldCount +newStart,newCount @@
			start, count := parseHunkNewRange(dl.Text)
			if start > 0 && line >= start && line < start+count {
				return i
			}
		}
	}

	// If we found the file but no matching hunk, return the file header.
	if fileHeaderIdx >= 0 {
		return fileHeaderIdx
	}
	return 0
}

// parseHunkNewRange extracts the new-file start and count from a hunk header.
// Input format: "@@ -oldStart,oldCount +newStart,newCount @@ optional context"
// Returns (start, count). Returns (0, 0) if parsing fails.
func parseHunkNewRange(header string) (int, int) {
	// Find "+start,count" or "+start" portion.
	idx := strings.Index(header, "+")
	if idx < 0 {
		return 0, 0
	}
	rest := header[idx+1:]
	end := strings.Index(rest, " ")
	if end < 0 {
		return 0, 0
	}
	part := rest[:end]

	if comma := strings.Index(part, ","); comma >= 0 {
		start, err1 := strconv.Atoi(part[:comma])
		count, err2 := strconv.Atoi(part[comma+1:])
		if err1 != nil || err2 != nil {
			return 0, 0
		}
		return start, count
	}

	start, err := strconv.Atoi(part)
	if err != nil {
		return 0, 0
	}
	return start, 1
}

// computeDiffLineNumbers assigns new-file line numbers to each diff line.
// Context and addition lines get incrementing new-file numbers; deletion,
// file header, and hunk header lines get 0 (no number shown).
func computeDiffLineNumbers(lines []diffLine) []int {
	nums := make([]int, len(lines))
	curLine := 0
	for i, dl := range lines {
		switch dl.Type {
		case diffLineHunkHeader:
			start, _ := parseHunkNewRange(dl.Text)
			curLine = start
		case diffLineContext:
			if curLine > 0 {
				nums[i] = curLine
				curLine++
			}
		case diffLineAddition:
			if curLine > 0 {
				nums[i] = curLine
				curLine++
			}
			// Deletion, file header: no line number (stays 0).
		}
	}
	return nums
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

	boxWidth := width
	if boxWidth < 20 {
		boxWidth = 80
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
	b.WriteString("\n\n")

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

	// Content width inside the box (2 border + 2 padding).
	contentWidth := boxWidth - 4
	if contentWidth < 1 {
		contentWidth = 1
	}

	// Compute new-file line numbers for each diff line.
	lineNumbers := computeDiffLineNumbers(lines)

	// Compute gutter width from max line number.
	maxLineNum := 0
	for _, n := range lineNumbers {
		if n > maxLineNum {
			maxLineNum = n
		}
	}
	gutterWidth := len(fmt.Sprintf("%d", maxLineNum))
	if gutterWidth < 1 {
		gutterWidth = 1
	}
	dimGutterStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Render visible lines, truncating to fit inside the box.
	// Insert blank line before file headers (except the first in the diff)
	// for visual separation between files.
	textWidth := contentWidth - gutterWidth - 1 // gutter + space
	if textWidth < 1 {
		textWidth = 1
	}
	for idx := offset; idx < end; idx++ {
		dl := lines[idx]
		if dl.Type == diffLineFileHeader && strings.HasPrefix(dl.Text, "diff --git") && idx > 0 {
			b.WriteString("\n")
		}

		// Line number gutter.
		if lineNumbers[idx] > 0 {
			gutter := fmt.Sprintf("%*d ", gutterWidth, lineNumbers[idx])
			b.WriteString(dimGutterStyle.Render(gutter))
		} else {
			b.WriteString(dimGutterStyle.Render(strings.Repeat(" ", gutterWidth+1)))
		}

		text := dl.Text
		if lipgloss.Width(text) > textWidth {
			text, _ = cutText(text, textWidth)
		}
		style := diffLineStyle(dl.Type)
		b.WriteString(style.Render(text))
		b.WriteString("\n")
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
	// Add scroll position indicator when content is scrollable.
	if viewHeight > 0 && len(lines) > viewHeight {
		title += fmt.Sprintf(" (%d/%d)", offset+1, len(lines))
	}

	return renderBoxWithFooter(title, b.String(), boxWidth, scrollHint)
}

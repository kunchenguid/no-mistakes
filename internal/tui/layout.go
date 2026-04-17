package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	responsiveLayoutMinWidth = 100
	responsiveLayoutGap      = 2
	responsiveLeftMinWidth   = 38
	responsiveLeftMaxWidth   = 48
	responsiveRightMinWidth  = 48

	// cappedPipelineHeight is the height passed to renderPipelineView when
	// an overlay (help) is active in non-responsive (stacked) layout.
	// Kept below 30 to suppress connector lines and save vertical space
	// for the overlay that stacks below.
	cappedPipelineHeight = 29
)

func joinSections(sections []string, gap string) string {
	filtered := make([]string, 0, len(sections))
	for _, section := range sections {
		if section != "" {
			filtered = append(filtered, section)
		}
	}
	return strings.Join(filtered, gap)
}

func hasResponsiveSidebarContent(m Model) bool {
	if m.err != nil || m.showHelp || isCIActive(m.steps) {
		return true
	}
	if awaitingStep(m.steps) != nil {
		return true
	}
	return len(m.logs) > 0
}

func shouldUseResponsiveLayout(width int, hasSidebarContent bool) bool {
	if !hasSidebarContent || width < responsiveLayoutMinWidth {
		return false
	}
	leftWidth, rightWidth := responsiveColumnWidths(width)
	return leftWidth >= responsiveLeftMinWidth && rightWidth >= responsiveRightMinWidth
}

func responsiveColumnWidths(width int) (int, int) {
	leftWidth := width / 3
	if leftWidth < responsiveLeftMinWidth {
		leftWidth = responsiveLeftMinWidth
	}
	if leftWidth > responsiveLeftMaxWidth {
		leftWidth = responsiveLeftMaxWidth
	}
	rightWidth := width - leftWidth - responsiveLayoutGap
	if rightWidth < responsiveRightMinWidth {
		rightWidth = responsiveRightMinWidth
		leftWidth = width - rightWidth - responsiveLayoutGap
	}
	return leftWidth, rightWidth
}

func renderResponsiveColumns(left, right string, leftWidth, rightWidth, gap int) string {
	if right == "" {
		return left
	}
	leftLines := strings.Split(left, "\n")
	rightLines := strings.Split(right, "\n")
	maxLines := len(leftLines)
	if len(rightLines) > maxLines {
		maxLines = len(rightLines)
	}

	leftStyle := lipgloss.NewStyle().Width(leftWidth)
	rightStyle := lipgloss.NewStyle().Width(rightWidth)
	gapStr := strings.Repeat(" ", gap)

	var b strings.Builder
	for i := 0; i < maxLines; i++ {
		leftLine := ""
		if i < len(leftLines) {
			leftLine = leftLines[i]
		}
		rightLine := ""
		if i < len(rightLines) {
			rightLine = rightLines[i]
		}
		b.WriteString(leftStyle.Render(leftLine))
		b.WriteString(gapStr)
		b.WriteString(rightStyle.Render(rightLine))
		if i < maxLines-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func sectionsHeight(sections []string, gapHeight int) int {
	count := 0
	height := 0
	for _, section := range sections {
		if section == "" {
			continue
		}
		if count > 0 {
			height += gapHeight
		}
		height += lipgloss.Height(section)
		count++
	}
	return height
}

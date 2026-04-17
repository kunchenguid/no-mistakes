package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderBox_HasRoundedCorners(t *testing.T) {
	out := renderBox("Title", "content", 40)
	if !strings.Contains(out, "╭") || !strings.Contains(out, "╮") {
		t.Error("expected rounded top corners ╭ and ╮")
	}
	if !strings.Contains(out, "╰") || !strings.Contains(out, "╯") {
		t.Error("expected rounded bottom corners ╰ and ╯")
	}
}

func TestRenderBox_TitleInTopBorder(t *testing.T) {
	out := stripANSI(renderBox("Pipeline", "step content", 40))
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(lines[0], "Pipeline") {
		t.Errorf("expected title 'Pipeline' in top border line, got %q", lines[0])
	}
}

func TestRenderBox_ContentInsideBorders(t *testing.T) {
	out := stripANSI(renderBox("Test", "hello world", 40))
	lines := strings.Split(out, "\n")
	// Find content line (between top and bottom border).
	foundContent := false
	for _, line := range lines[1:] {
		if strings.Contains(line, "hello world") {
			foundContent = true
			// Content lines should start with │.
			if !strings.HasPrefix(strings.TrimSpace(line), "│") {
				t.Errorf("expected content line to start with │, got %q", line)
			}
			break
		}
	}
	if !foundContent {
		t.Error("expected 'hello world' inside box")
	}
}

func TestRenderBox_HorizontalPadding(t *testing.T) {
	out := stripANSI(renderBox("Test", "X", 20))
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, "X") {
			// Content should have at least 1 space padding from border.
			if strings.Contains(line, "│X") || strings.Contains(line, "X│") {
				t.Errorf("expected horizontal padding between content and border, got %q", line)
			}
			break
		}
	}
}

func TestRenderBox_FillsWidth(t *testing.T) {
	out := stripANSI(renderBox("Title", "content", 50))
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w != 50 {
			t.Errorf("expected line width 50, got %d for line %q", w, line)
		}
	}
}

func TestRenderBox_MultilineContent(t *testing.T) {
	out := stripANSI(renderBox("Test", "line1\nline2\nline3", 40))
	if !strings.Contains(out, "line1") || !strings.Contains(out, "line2") || !strings.Contains(out, "line3") {
		t.Error("expected all content lines in output")
	}
}

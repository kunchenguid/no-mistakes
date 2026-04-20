package wizard

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

func hasLineContainingAll(view string, needles ...string) bool {
	for _, line := range strings.Split(stripANSI(view), "\n") {
		match := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lineContaining(view string, needle string) string {
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func TestView_RendersSetupTitle(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.width = 80
	out := m.View()
	if !strings.Contains(out, "Setup") {
		t.Fatalf("expected box title 'Setup', got:\n%s", out)
	}
}

func TestView_RendersAllStepLabels(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.width = 80
	out := m.View()
	for _, label := range []string{"Branch", "Commit", "Push"} {
		if !strings.Contains(out, label) {
			t.Errorf("expected %q in view, got:\n%s", label, out)
		}
	}
}

func TestView_ShowsSkipReason(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/existing"
	cfg.NeedsBranch = false
	m := NewModel(cfg)
	m.width = 80
	out := m.View()
	if !strings.Contains(out, "feat/existing") {
		t.Fatalf("expected skip reason to mention branch, got:\n%s", out)
	}
}

func TestView_FooterReflectsSideEffects(t *testing.T) {
	r := &recorder{}
	m := NewModel(baseConfig(r))
	m.width = 80

	// No side-effects yet: footer shows "q quit".
	initial := m.View()
	if !strings.Contains(initial, "quit") {
		t.Fatalf("expected 'quit' in initial footer, got:\n%s", initial)
	}

	// After creating a branch, footer should mention side-effects stay.
	m = advance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("feat/x")})
	m = advance(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.width = 80
	after := m.View()
	if !strings.Contains(after, "abort") {
		t.Fatalf("expected 'abort' in footer after side-effects, got:\n%s", after)
	}
	if !strings.Contains(after, "feat/x") {
		t.Fatalf("expected branch name in footer, got:\n%s", after)
	}
}

func TestView_ActionBarForConfirm(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/x"
	cfg.NeedsBranch = false
	cfg.IsDirty = false
	m := NewModel(cfg)
	m.width = 80
	out := m.View()
	if !strings.Contains(out, "push") {
		t.Fatalf("expected 'push' in action bar for confirm step, got:\n%s", out)
	}
}

func TestView_RendersInputInlineWithPlaceholder(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.width = 80

	out := m.View()
	if !hasLineContainingAll(out, "Branch", "›", "blank = let agent suggest") {
		t.Fatalf("expected inline input placeholder on Branch line, got:\n%s", stripANSI(out))
	}
	if strings.Contains(stripANSI(out), "branch name (blank to let the agent suggest):") {
		t.Fatalf("expected stacked branch input label to be removed, got:\n%s", stripANSI(out))
	}
}

func TestView_InlineInputFitsInsideBoxAtMinimumWidth(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.width = 60
	m.input.SetValue(strings.Repeat("x", 80))

	out := m.View()
	branchLine := lineContaining(out, "Branch")
	if branchLine == "" {
		t.Fatalf("expected branch line in view, got:\n%s", stripANSI(out))
	}
	if got := lipgloss.Width(branchLine); got > 60 {
		t.Fatalf("expected inline input line width <= 60, got %d:\n%s", got, stripANSI(out))
	}
}

func TestView_RendersTransientStatusInlineWithStep(t *testing.T) {
	tests := []struct {
		name   string
		status stepStatus
		result string
		want   string
	}{
		{name: "agent", status: statAgent, want: "asking agent for a branch name"},
		{name: "running", status: statRunning, result: "feat/wizard", want: "creating branch feat/wizard"},
		{name: "confirm", status: statConfirm, want: "push main to no-mistakes gate?"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModel(baseConfig(&recorder{}))
			m.width = 120
			m.steps[0].status = tt.status
			m.steps[0].result = tt.result

			out := m.View()
			if !hasLineContainingAll(out, "Branch", tt.want) {
				t.Fatalf("expected Branch and %q on the same line, got:\n%s", tt.want, stripANSI(out))
			}
		})
	}
}

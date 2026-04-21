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

func TestView_EmitsTerminalTitleEscapeSequence(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.width = 80
	out := m.View()
	if !strings.HasPrefix(out, "\x1b]2;") {
		t.Fatalf("expected View output to start with OSC 2 terminal title sequence, got: %q", out[:min(40, len(out))])
	}
	// Title must end with BEL before the rest of the rendered UI.
	end := strings.Index(out, "\x07")
	if end < 0 {
		t.Fatalf("expected BEL terminator in terminal title sequence, got: %q", out)
	}
}

func TestTerminalTitle_BranchInputIncludesCurrentBranch(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	got := m.terminalTitle()
	if !strings.Contains(got, "Branch") {
		t.Fatalf("expected title to mention Branch step, got: %q", got)
	}
	if !strings.Contains(got, "main") {
		t.Fatalf("expected title to mention current branch %q, got: %q", "main", got)
	}
	if !strings.Contains(got, "Setup") {
		t.Fatalf("expected title to include Setup prefix, got: %q", got)
	}
}

func TestTerminalTitle_PushConfirmUsesTargetBranch(t *testing.T) {
	cfg := baseConfig(&recorder{})
	cfg.CurrentBranch = "feat/x"
	cfg.NeedsBranch = false
	cfg.IsDirty = false
	m := NewModel(cfg)
	got := m.terminalTitle()
	if !strings.Contains(got, "Push") {
		t.Fatalf("expected title to mention Push step, got: %q", got)
	}
	if !strings.Contains(got, "feat/x") {
		t.Fatalf("expected title to include target branch, got: %q", got)
	}
}

func TestTerminalTitle_SuccessShowsComplete(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.success = true
	m.pushed = true
	m.targetBranch = "feat/done"
	got := m.terminalTitle()
	if !strings.Contains(got, "complete") {
		t.Fatalf("expected title to indicate completion, got: %q", got)
	}
	if !strings.Contains(got, "feat/done") {
		t.Fatalf("expected title to include target branch on success, got: %q", got)
	}
}

func TestTerminalTitle_AbortedShowsAborted(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.aborted = true
	got := m.terminalTitle()
	if !strings.Contains(got, "aborted") {
		t.Fatalf("expected title to indicate aborted, got: %q", got)
	}
}

func TestTerminalTitle_FailedShowsStepName(t *testing.T) {
	m := NewModel(baseConfig(&recorder{}))
	m.steps[0].status = statFailed
	m.steps[0].errMsg = "boom"
	got := m.terminalTitle()
	if !strings.Contains(got, "Branch") {
		t.Fatalf("expected failed title to mention failing step, got: %q", got)
	}
	if !strings.Contains(got, "✗") {
		t.Fatalf("expected failed title to include ✗ icon, got: %q", got)
	}
}

func TestSetTerminalTitle_FormatsOSCSequence(t *testing.T) {
	got := setTerminalTitle("hello")
	want := "\x1b]2;hello\x07"
	if got != want {
		t.Fatalf("setTerminalTitle: got %q, want %q", got, want)
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

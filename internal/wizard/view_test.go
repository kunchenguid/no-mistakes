package wizard

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

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

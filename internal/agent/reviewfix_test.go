package agent

import (
	"context"
	"testing"
)

type reviewRoleTestAgent struct {
	name      string
	calls     int
	closed    int
	resumable bool
}

func (a *reviewRoleTestAgent) Name() string { return a.name }
func (a *reviewRoleTestAgent) Run(_ context.Context, _ RunOpts) (*Result, error) {
	a.calls++
	return &Result{Text: a.name}, nil
}
func (a *reviewRoleTestAgent) Close() error { a.closed++; return nil }
func (a *reviewRoleTestAgent) SupportsSessionResume() bool {
	return a.resumable
}
func (a *reviewRoleTestAgent) SupportsSessionProvider(provider string) bool {
	return a.resumable && provider == a.name
}

func TestReviewFixSelectionKeepsDefaultRunAndSelectsFixerExplicitly(t *testing.T) {
	primary := &reviewRoleTestAgent{name: "claude"}
	fixer := &reviewRoleTestAgent{name: "pi", resumable: true}
	selected := NewReviewFixSelection(primary, fixer)

	if _, err := selected.Run(context.Background(), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := AgentForReviewFix(selected).Run(context.Background(), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 1 || fixer.calls != 1 {
		t.Fatalf("calls = primary %d, fixer %d; want one each", primary.calls, fixer.calls)
	}
	if !SupportsSessionProvider(selected, "pi") || SupportsSessionProvider(selected, "claude") {
		t.Fatal("combined selection must expose only the Review fixer's session providers")
	}
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	if primary.closed != 1 || fixer.closed != 1 {
		t.Fatalf("close counts = primary %d, fixer %d; want one each", primary.closed, fixer.closed)
	}
}

func TestReviewFixSelectionAbsentPreservesAgentIdentity(t *testing.T) {
	primary := &reviewRoleTestAgent{name: "claude"}
	selected := NewReviewFixSelection(primary, nil)
	if selected != primary || AgentForReviewFix(selected) != primary || HasReviewFixSelection(selected) {
		t.Fatal("an absent Review-fixer override must preserve the original agent")
	}
}

package agent

import (
	"context"
	"testing"
)

type roleRecorder struct {
	name               string
	calls              []RunOpts
	closed             int
	resumable, neutral bool
}

func (a *roleRecorder) Name() string { return a.name }
func (a *roleRecorder) Run(_ context.Context, opts RunOpts) (*Result, error) {
	a.calls = append(a.calls, opts)
	return &Result{SessionID: "fix-session"}, nil
}
func (a *roleRecorder) Close() error                      { a.closed++; return nil }
func (a *roleRecorder) SupportsSessionResume() bool       { return a.resumable }
func (a *roleRecorder) NeutralizesGateInstructions() bool { return a.neutral }

func TestReviewAgentsRouteAndPreserveCapabilities(t *testing.T) {
	primary := &roleRecorder{name: "codex", neutral: true}
	reviewer := &roleRecorder{name: "claude", neutral: true, resumable: true}
	fixer := &roleRecorder{name: "pi", neutral: true, resumable: true}
	ag := WithReviewAgents(primary, reviewer, fixer)
	var attempts []string
	for _, purpose := range []string{"review", "review-fix", "review-correction", "test-evidence", "review-fix"} {
		result, err := ag.Run(context.Background(), RunOpts{Purpose: purpose, Session: &SessionRef{ID: "prior", Agent: "pi"}, OnAttempt: func(a Attempt) { attempts = append(attempts, a.Agent) }})
		if err != nil {
			t.Fatal(err)
		}
		if result.Provider == "" {
			t.Fatal("concrete provider was lost")
		}
	}
	if len(primary.calls) != 1 || len(reviewer.calls) != 2 || len(fixer.calls) != 2 {
		t.Fatal("incorrect role routing")
	}
	for _, call := range reviewer.calls {
		if call.Session != nil {
			t.Fatal("review inherited a session")
		}
	}
	for _, call := range fixer.calls {
		if call.Session == nil || call.Session.ID != "prior" {
			t.Fatal("fix session lost")
		}
	}
	if len(attempts) != 5 || attempts[0] != "claude" || attempts[1] != "pi" || attempts[3] != "codex" {
		t.Fatalf("attempts = %v", attempts)
	}
	if !SupportsSessionResume(ag) || !SupportsSessionProvider(ag, "pi") || SupportsSessionProvider(ag, "claude") {
		t.Fatal("sessions must follow fixer capability only")
	}
	if !NeutralizesGateInstructions(ag) {
		t.Fatal("neutralization not forwarded")
	}
	reviewer.neutral = false
	if NeutralizesGateInstructions(ag) {
		t.Fatal("unsafe reviewer accepted")
	}
	if err := ag.Close(); err != nil {
		t.Fatal(err)
	}
	if primary.closed != 1 || reviewer.closed != 1 || fixer.closed != 1 {
		t.Fatal("agents must close exactly once")
	}
}

func TestReviewAgentsDefaults(t *testing.T) {
	primary := &roleRecorder{name: "pi", resumable: true}
	if WithReviewAgents(primary, nil, nil) != primary {
		t.Fatal("absent roles must preserve original agent")
	}
	reviewer := &roleRecorder{name: "claude"}
	ag := WithReviewAgents(primary, reviewer, nil)
	if !SupportsSessionProvider(ag, "pi") {
		t.Fatal("default fixer capability lost")
	}
	_, err := ag.Run(context.Background(), RunOpts{Purpose: "review-fix"})
	if err != nil || len(primary.calls) != 1 {
		t.Fatal("unset fixer did not use primary")
	}
	ag = WithReviewAgents(primary, nil, &roleRecorder{name: "cold"})
	if SupportsSessionResume(ag) {
		t.Fatal("nonresumable fixer inherited primary capability")
	}
}

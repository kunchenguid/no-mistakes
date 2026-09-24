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
	for _, purpose := range []string{"review", "review-fix", "review", "test-evidence", "review-fix"} {
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

func TestReviewRolesHandOverAtConfiguredRound(t *testing.T) {
	primary := &roleRecorder{name: "codex", neutral: true, resumable: true}
	fixer := &roleRecorder{name: "pi", neutral: true, resumable: true}
	lateFixer := &roleRecorder{name: "cheap", neutral: true, resumable: true}
	lateReviewer := &roleRecorder{name: "late-review", neutral: true}
	ag := WithReviewRoles(primary, ReviewRoles{
		Reviewer: RoundedRole{Late: lateReviewer, LateFrom: 2},
		Fixer:    RoundedRole{Agent: fixer, Late: lateFixer, LateFrom: 3},
	})
	for _, round := range []int{1, 2, 3, 4} {
		for _, purpose := range []string{"review", "review-fix"} {
			if _, err := ag.Run(context.Background(), RunOpts{Purpose: purpose, Round: round}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Reviewer: round 1 has no configured base role, so it stays on the
	// default agent; rounds 2+ move to the overlay.
	if len(lateReviewer.calls) != 3 {
		t.Fatalf("late reviewer served %d rounds, want 3", len(lateReviewer.calls))
	}
	// Fixer: rounds 1-2 on the configured fixer, rounds 3-4 on the overlay.
	if len(fixer.calls) != 2 || len(lateFixer.calls) != 2 {
		t.Fatalf("fix rounds split %d/%d, want 2/2", len(fixer.calls), len(lateFixer.calls))
	}
	// One primary call: the round-1 review that has no base reviewer.
	if len(primary.calls) != 1 {
		t.Fatalf("primary served %d invocations, want 1", len(primary.calls))
	}
	// An invocation the pipeline did not number must not be routed by a
	// round the router had to guess.
	if _, err := ag.Run(context.Background(), RunOpts{Purpose: "review-fix"}); err != nil {
		t.Fatal(err)
	}
	if len(fixer.calls) != 3 || len(lateFixer.calls) != 2 {
		t.Fatal("unnumbered invocation left the primary role")
	}
	// A later-round fixer that cannot resume must close the capability for
	// the whole run rather than letting round 1 advertise it.
	if !SupportsSessionResume(ag) {
		t.Fatal("all-resumable fixers lost session capability")
	}
	lateFixer.resumable = false
	if SupportsSessionResume(ag) || SupportsSessionProvider(ag, "pi") {
		t.Fatal("nonresumable later-round fixer inherited the early fixer capability")
	}
	if !NeutralizesGateInstructions(ag) {
		t.Fatal("neutralization not forwarded across overlays")
	}
	lateReviewer.neutral = false
	if NeutralizesGateInstructions(ag) {
		t.Fatal("unsafe later-round reviewer accepted")
	}
	if err := ag.Close(); err != nil {
		t.Fatal(err)
	}
	if primary.closed != 1 || fixer.closed != 1 || lateFixer.closed != 1 || lateReviewer.closed != 1 {
		t.Fatal("overlay agents must close exactly once")
	}
}

func TestReviewRolesWithoutOverlaysIgnoreRounds(t *testing.T) {
	primary := &roleRecorder{name: "codex"}
	fixer := &roleRecorder{name: "pi"}
	ag := WithReviewAgents(primary, nil, fixer)
	for _, round := range []int{1, 2, 9} {
		if _, err := ag.Run(context.Background(), RunOpts{Purpose: "review-fix", Round: round}); err != nil {
			t.Fatal(err)
		}
	}
	if len(fixer.calls) != 3 || len(primary.calls) != 0 {
		t.Fatal("unconfigured overlay changed routing by round")
	}
	if WithReviewRoles(primary, ReviewRoles{}) != primary {
		t.Fatal("empty roles must preserve the original agent")
	}
	// An overlay agent with no round to take over from never serves.
	late := &roleRecorder{name: "late"}
	ag = WithReviewRoles(primary, ReviewRoles{Fixer: RoundedRole{Agent: fixer, Late: late}})
	if _, err := ag.Run(context.Background(), RunOpts{Purpose: "review-fix", Round: 7}); err != nil {
		t.Fatal(err)
	}
	if len(late.calls) != 0 || len(fixer.calls) != 4 {
		t.Fatal("overlay without a takeover round served a turn")
	}
}

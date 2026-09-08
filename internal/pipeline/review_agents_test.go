package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestReviewRoleRoutingPersistsAndRecoversOnlyFixerSession(t *testing.T) {
	database, run := sessionTestDB(t)
	primary, reviewer, fixer := newFakeSessionAgent(), newFakeSessionAgent(), newFakeSessionAgent()
	primary.name, reviewer.name, fixer.name = "primary", "reviewer", "fixer"
	routed := agent.WithReviewAgents(primary, reviewer, fixer)
	sessions := NewRunSessions(database, run.ID, routed, true)
	for i := 0; i < 2; i++ {
		// Mirrors fresh review turns and resumable fixes through the step timeout wrapper.
		sctx := &StepContext{Agent: routed, Sessions: sessions}
		if _, err := sctx.RunAgentContext(context.Background(), agent.RunOpts{Purpose: "review"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sctx.RunAgentSessionContext(context.Background(), SessionRoleFixer, agent.RunOpts{Purpose: "review-fix"}); err != nil {
			t.Fatal(err)
		}
		sessions = NewRunSessions(database, run.ID, routed, true)
	}
	if len(primary.calls) != 0 || len(reviewer.calls) != 2 || len(fixer.calls) != 2 {
		t.Fatal("wrong role dispatch")
	}
	for _, call := range reviewer.calls {
		if call.session != nil {
			t.Fatal("review resumed a session")
		}
	}
	if fixer.calls[0].session == nil || fixer.calls[0].session.ID != "" || fixer.calls[1].session == nil || fixer.calls[1].session.ID != "sess-1" {
		t.Fatalf("fix calls = %+v", fixer.calls)
	}
	stored, err := database.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Agent != "fixer" || stored[0].Role != string(SessionRoleFixer) {
		t.Fatalf("persisted sessions = %+v", stored)
	}
}

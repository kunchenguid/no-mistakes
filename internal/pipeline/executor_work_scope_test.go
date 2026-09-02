package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_DocumentCompletionCarriesCombinedHousekeepingScope(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if _, err := database.InsertAgentInvocation(db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepDocument), Round: 1,
		Purpose: "housekeeping", Agent: "codex", SessionMode: db.InvocationModeCold,
		StartedAt: 1, CompletedAt: 2, DurationMS: 1000, ExitStatus: "ok",
	}); err != nil {
		t.Fatalf("insert invocation: %v", err)
	}

	events := &eventCollector{}
	executor := NewExecutor(database, p, nil, nil, nil, events.handler)
	duration := int64(1200)
	executor.emitStepEventWithFindingsAndError(
		ipc.EventStepCompleted, run, repo, types.StepDocument,
		string(types.StepStatusCompleted), "", "", &duration,
	)

	event := events.find(ipc.EventStepCompleted, types.StepDocument)
	if event == nil {
		t.Fatal("document completion event not emitted")
	}
	if event.WorkScope != ipc.WorkScopeDocumentLintHousekeeping {
		t.Fatalf("work scope = %q, want combined housekeeping", event.WorkScope)
	}
}

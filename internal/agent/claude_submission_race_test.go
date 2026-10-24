package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Under a still-working cap above the silent budget, the pipeline seam stops
// a turn past its silent budget once it has been quiet for its quiet stop,
// which for a budget this short is the budget itself. A submission accepted
// late in the silent budget and then followed by silence must end the
// session first, so the seam returns the submission instead of a timeout.
func TestClaudeAgent_IdleEndBeatsThePipelineQuietStopUnderAWorkingCap(t *testing.T) {
	const (
		silentBudget = 7 * time.Second
		workingCap   = time.Minute
	)
	agent.WithClaudeQuietStop(t, silentBudget, 500*time.Millisecond)
	errBudget := errors.New("review agent timed out")
	sctx := &pipeline.StepContext{
		Ctx:   context.Background(),
		Agent: agent.NewClaudeLateSubmitHelperAgent(t, 4*time.Second),
	}

	res, err := sctx.RunAgentBudget(context.Background(), silentBudget, workingCap, errBudget, agent.RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("RunAgentBudget: %v, want the late submission kept", err)
	}
	if string(res.Output) != agent.ClaudeEarlyPartialSubmission {
		t.Fatalf("output = %s, want the late partial submission", res.Output)
	}
}

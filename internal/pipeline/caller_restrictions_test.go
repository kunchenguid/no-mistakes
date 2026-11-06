package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// Decode the durable wire shape so this regression also runs before the new
// run field exists. The old reader silently discards it and loses the bounds.
func TestRunAgentCallerRestrictionsReachEveryTurn(t *testing.T) {
	var run db.Run
	err := json.Unmarshal([]byte(`{"ID":"run-one","RepoID":"repo-one","Branch":"feature","HeadSHA":"current-head","SubmittedHeadSHA":"submitted-head","CallerRestrictions":{"source":"caller --caller-restrictions","source_path":"/private/caller.txt","content":"Never rerun a suite or install dependencies.\n","sha256":"5b4b87be55d7333566f305ca37ede5388861fc932d72bf703b3d80e2d1d617e65","captured_at":1}}`), &run)
	if err != nil {
		t.Fatal(err)
	}
	ag := &hangingAgent{name: "fake", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		for _, want := range []string{"Never rerun a suite or install dependencies.\n", "caller --caller-restrictions", "run-one", "submitted-head", "current-head", "trusted configuration", "report"} {
			if !strings.Contains(opts.Prompt, want) {
				t.Errorf("actual invocation input lost %q: %s", want, opts.Prompt)
			}
		}
		return &agent.Result{Text: "ok"}, nil
	}}
	sctx := &StepContext{Ctx: context.Background(), Run: &run, Agent: ag}
	for i := 0; i < 2; i++ {
		if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "perform step"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sctx.RunAgentSessionContext(context.Background(), SessionRoleFixer, agent.RunOpts{Prompt: "continue step"}); err != nil {
			t.Fatal(err)
		}
	}
}

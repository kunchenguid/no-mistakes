package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// The Jev review pre-brief (jev.review_assist, with its optional
// jev.candidate_excerpt_bytes excerpt) was retired: offline measurement showed
// the candidate generator excludes changed files by construction while nearly
// all recorded finding locations are changed files, so its ranked listing
// could not reach what it ranks for. The review step runs the same cold,
// session-free, complete review it always ran without the assist, and the
// prompt must carry no pre-brief section - even when a TYPESAFE_API_KEY is
// still present in the environment, the step must not consult TypeSafe.
func TestReviewStep_RunsColdWithoutTheRetiredJevPrebrief(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "nm-retired-prebrief-must-not-be-used")

	dir, baseSHA, headSHA := setupGitRepo(t)
	var prompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			prompt = opts.Prompt
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want a clean approval with no parked gate", outcome)
	}
	for _, banned := range []string{"Pre-brief (advisory output", "jev", "TYPESAFE_API_KEY"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("review prompt mentions the retired pre-brief (%q):\n%s", banned, prompt)
		}
	}
}

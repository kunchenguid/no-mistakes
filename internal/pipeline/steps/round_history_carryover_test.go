package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// scopeLimitedFakeStep stands in for ReviewStep in a real executor run: it
// declares that its rounds can be scope limited (so the executor carries
// unresolved findings across them) and returns a scripted outcome per round.
type scopeLimitedFakeStep struct {
	outcomes []*pipeline.StepOutcome
	calls    int
}

func (s *scopeLimitedFakeStep) Name() types.StepName { return types.StepReview }

func (s *scopeLimitedFakeStep) FindingsMayBeScopeLimited() bool { return true }

func (s *scopeLimitedFakeStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.calls < len(s.outcomes) {
		outcome := s.outcomes[s.calls]
		s.calls++
		return outcome, nil
	}
	s.calls++
	return &pipeline.StepOutcome{}, nil
}

// TestRoundHistoryPromptSection_CarriedFindingSelectedForFixIsNotRenderedAsIgnored
// drives a real executor through the carry-forward shape and then renders the
// history the NEXT fix agent is given.
//
// Round 1 reports a nit and a blocker; the operator fixes only the nit, so the
// blocker carries as review-2. Round 2's rereview restates the blocker, which
// normalizes to its own positional review-1 before the merge stamps the
// carried review-2 back onto it, and the operator answers that gate by naming
// review-2. If the round record still calls that finding review-1, the
// recorded selection matches nothing in it and the blocker renders under
// user_chose_to_ignore - a section this prompt explicitly tells the agent not
// to re-report, i.e. the fix agent is instructed to leave alone the one
// finding the operator asked to have fixed.
func TestRoundHistoryPromptSection_CarriedFindingSelectedForFixIsNotRenderedAsIgnored(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}

	const blocker = "unchecked error swallows the failure"
	round1 := `{"findings":[
		{"id":"review-1","severity":"warning","file":"a.go","line":3,"description":"stale comment","action":"ask-user"},
		{"id":"review-2","severity":"error","file":"b.go","line":9,"description":"` + blocker + `","action":"ask-user"}
	],"summary":"2 findings"}`
	round2 := `{"findings":[
		{"id":"review-1","severity":"error","file":"b.go","line":9,"description":"` + blocker + `","action":"ask-user"}
	],"summary":"1 finding"}`

	step := &scopeLimitedFakeStep{outcomes: []*pipeline.StepOutcome{
		{NeedsApproval: true, Findings: round1},
		{NeedsApproval: true, Findings: round2},
	}}
	exec := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, t.TempDir())
	}()

	respond := func(ids []string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := exec.Respond(types.StepReview, types.ActionFix, ids); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("gate never accepted a response for %v", ids)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Round 1's gate: fix the nit, leave the blocker.
	respond([]string{"review-1"})
	// Round 2's gate: fix the blocker under the ID the gate showed.
	respond([]string{"review-2"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{DB: database, StepResultID: steps[0].ID}
	history := roundHistoryPromptSection(sctx)
	if history == "" {
		t.Fatal("expected a rendered round history")
	}

	round2Section := history[strings.Index(history, "Round 2 "):]
	if idx := strings.Index(round2Section, "Round 3 "); idx >= 0 {
		round2Section = round2Section[:idx]
	}
	ignored := sectionAfter(round2Section, "user_chose_to_ignore:")
	if strings.Contains(ignored, blocker) {
		t.Fatalf("the operator selected the carried blocker to fix, but round 2's history tells the next agent it was ignored:\n%s", round2Section)
	}
	chosen := sectionAfter(round2Section, "user_chose_to_fix:")
	if !strings.Contains(chosen, blocker) {
		t.Fatalf("round 2's history must show the carried blocker as selected to fix:\n%s", round2Section)
	}
}

// sectionAfter returns the part of text following header, up to the next
// top-level key line, or "" when header is absent.
func sectionAfter(text, header string) string {
	idx := strings.Index(text, header)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(header):]
	for _, next := range []string{"\nuser_chose_to_fix:", "\nuser_chose_to_ignore:", "\nauto_selected_to_fix:", "\nfindings:"} {
		if at := strings.Index(rest, next); at >= 0 {
			rest = rest[:at]
		}
	}
	return rest
}

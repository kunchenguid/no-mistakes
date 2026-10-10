package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// Reproduction: a finding the user chose to fix in round 1 is carried into the
// round 2 gate, and a round 2 `respond --action fix --findings` that omits it
// re-renders that same finding under user_chose_to_ignore. Later rounds then
// see the user both fixing and ignoring one finding, and the ignore guidance
// tells them not to keep code changed to satisfy it.
func TestRoundHistory_OmittedCarriedFindingIsNotRecordedAsIgnored(t *testing.T) {
	t.Parallel()
	sctx, stepID := newRoundHistoryContext(t)

	// Round 1: one ask-user and one auto-fix finding; the user fixes both.
	round1 := `{"findings":[` +
		`{"id":"R1","severity":"error","file":"a.go","description":"remove the --allow-dirty bypass","action":"ask-user"},` +
		`{"id":"R2","severity":"error","file":"a.go","description":"set -u crash in review","action":"auto-fix"}` +
		`],"summary":"2"}`
	r1, err := sctx.DB.InsertStepRound(stepID, 1, "initial", &round1, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	both := `["R1","R2"]`
	if err := sctx.DB.SetStepRoundSelection(r1.ID, &both, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	// Round 2: the fix-review gate carries R1 and R2 and adds R3. The response
	// lists only the auto-fix ids, omitting the already-fixed ask-user R1.
	round2 := `{"findings":[` +
		`{"id":"R1","severity":"error","file":"a.go","description":"remove the --allow-dirty bypass","action":"ask-user"},` +
		`{"id":"R2","severity":"error","file":"a.go","description":"set -u crash in review","action":"auto-fix"},` +
		`{"id":"R3","severity":"warning","file":"b.go","description":"dollar escaping in compose","action":"auto-fix"}` +
		`],"summary":"3"}`
	r2, err := sctx.DB.InsertStepRound(stepID, 2, "auto_fix", &round2, nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	partial := `["R2","R3"]`
	if err := sctx.DB.SetStepRoundSelection(r2.ID, &partial, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	got := roundHistoryPromptSection(sctx)
	t.Logf("history rendered into the next round's prompt:\n%s", got)

	round2Block := got[strings.Index(got, "Round 2 (auto_fix)"):]
	ignored := ""
	if i := strings.Index(round2Block, "user_chose_to_ignore:"); i >= 0 {
		ignored = round2Block[i:]
	}
	if strings.Contains(ignored, "remove the --allow-dirty bypass") {
		t.Errorf("R1 was chosen to fix in round 1, yet omitting it from round 2's --findings renders it under user_chose_to_ignore")
	}
}

func TestRoundHistory_ReusedIDDeclineSurvivesEarlierSelection(t *testing.T) {
	t.Parallel()
	sctx, stepID := newRoundHistoryContext(t)
	for i, description := range []string{"old defect", "new defect"} {
		raw := `{"findings":[{"id":"R1","description":"` + description + `"}]}`
		r, err := sctx.DB.InsertStepRound(stepID, i+1, "initial", &raw, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			selected := `["R1"]`
			err = sctx.DB.SetStepRoundSelection(r.ID, &selected, db.RoundSelectionSourceUser)
		} else {
			err = sctx.DB.SetStepRoundDeclined(r.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	rounds, err := sctx.DB.GetRoundsByStep(stepID)
	if err != nil {
		t.Fatal(err)
	}
	blocks := renderRoundHistoryBlocks(rounds)
	if !strings.Contains(blocks[1], "user_chose_to_ignore:") {
		t.Fatalf("new decline suppressed: %s", blocks[1])
	}
}

func TestRoundHistory_BranchWindowKeepsPredecessorSelection(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	raw := `{"findings":[{"id":"R1","description":"authorized repair"}]}`
	for i := 1; i <= db.MaxBranchDecisionRounds+1; i++ {
		r, err := f.db.InsertStepRound(f.reviewSR.ID, i, "initial", &raw, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			selected := `["R1"]`
			err = f.db.SetStepRoundSelection(r.ID, &selected, db.RoundSelectionSourceUser)
		} else {
			err = f.db.SetStepRoundDeclined(r.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, truncated, err := f.db.GetBranchDecisionRounds(f.repo.ID, f.run.Branch, "next-run", db.MaxBranchDecisionRounds)
	if err != nil || !truncated {
		t.Fatalf("window: %v, %v", truncated, err)
	}
	sctx := f.testStepContext()
	sctx.PriorBranchDecisions = entries
	got := branchDecisionsPromptSection(sctx)
	if strings.Contains(got, "declined:") {
		t.Fatalf("earlier fix became declined beyond window: %s", got)
	}
}

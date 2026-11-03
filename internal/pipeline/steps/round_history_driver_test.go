package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// A driver's fix guidance scoped one round. The step's own history shows it
// under its own name and says whose it is; the decision sections, which reach
// other steps and later runs as the user's choices, never carry it.
func TestDriverInstructionsAreNeverPresentedAsTheUsersDecision(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	const guidance = "keep the start-of-run comparison as the only gate"
	findings := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","description":"stale comparison","action":"auto-fix"}]}`
	round, err := f.db.InsertStepRound(f.reviewSR.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["review-1"]`
	decided := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","description":"stale comparison","action":"auto-fix","driver_instructions":"` + guidance + `"}]}`
	if err := f.db.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceUser, &decided); err != nil {
		t.Fatal(err)
	}

	own := roundHistoryPromptSection(&pipeline.StepContext{DB: f.db, Repo: f.repo, Run: f.run, StepResultID: f.reviewSR.ID})
	if !strings.Contains(own, `"driver_instructions":"`+guidance+`"`) {
		t.Fatalf("own-step history lost the driver guidance:\n%s", own)
	}
	if strings.Contains(own, "user_instructions") {
		t.Fatalf("own-step history presents driver guidance as user instructions:\n%s", own)
	}
	if !strings.Contains(own, driverInstructionsNote) {
		t.Fatalf("own-step history does not say whose the guidance is:\n%s", own)
	}

	other := roundHistoryPromptSection(f.testStepContext())
	if !strings.Contains(other, "review round 1 user chose to fix") {
		t.Fatalf("the selection itself must still reach other steps:\n%s", other)
	}
	if strings.Contains(other, guidance) {
		t.Fatalf("a decision line carries the driver's guidance as the user's choice:\n%s", other)
	}
}

// A later run sees an earlier run's rounds as history, not as its own fix
// round, so the driver's guidance that scoped that round must not ride along
// under user_chose_to_fix.
func TestEarlierRunRoundsNeverCarryDriverInstructions(t *testing.T) {
	t.Parallel()
	const guidance = "only touch a.go"
	findings := `{"findings":[{"id":"R1","severity":"error","file":"a.go","description":"stale comparison","action":"auto-fix"}]}`
	selected := `["R1"]`
	decided := `{"findings":[{"id":"R1","severity":"error","file":"a.go","description":"stale comparison","action":"auto-fix","driver_instructions":"` + guidance + `"}]}`
	source := db.RoundSelectionSourceUser
	rounds := []*db.StepRound{{
		Round:              1,
		Trigger:            "initial",
		FindingsJSON:       &findings,
		SelectedFindingIDs: &selected,
		SelectionSource:    &source,
		UserFindingsJSON:   &decided,
	}}

	for name, section := range map[string]string{
		"superseded":  supersededReviewHistoryPromptSection(&pipeline.StepContext{PreviousRunReviewRounds: rounds}),
		"uncertified": uncertifiedRoundHistoryPromptSection(&pipeline.StepContext{UncertifiedPriorRounds: rounds}),
	} {
		if !strings.Contains(section, "user_chose_to_fix:") || !strings.Contains(section, "stale comparison") {
			t.Fatalf("%s: the user's selection itself must still be shown:\n%s", name, section)
		}
		if strings.Contains(section, guidance) || strings.Contains(section, "driver_instructions") {
			t.Fatalf("%s: driver guidance is rendered inside the user's decision:\n%s", name, section)
		}
	}
}

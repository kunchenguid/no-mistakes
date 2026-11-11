package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExternalDecisionReachesEveryHistoryChannel(t *testing.T) {
	f := newDecisionFixture(t)
	shown := `{"findings":[{"id":"external-A","description":"external fact"},{"id":"undecided-B","description":"still unresolved"}]}`
	subset := `{"findings":[{"id":"external-A","description":"external fact","user_instructions":"verified outside worktree","driver_instructions":"private driver note"}]}`
	round, err := f.db.InsertStepRound(f.reviewSR.ID, 1, "initial", &shown, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	empty := db.DeclinedSelectionJSON
	if err := f.db.SetStepRoundUserDecision(round.ID, &empty, db.RoundSelectionSourceExternal, &subset, types.ResponseOriginInteractive); err != nil {
		t.Fatal(err)
	}
	own := &pipeline.StepContext{DB: f.db, Repo: f.repo, Run: f.run, StepResultID: f.reviewSR.ID}
	laterRun, err := f.db.InsertRun(f.repo.ID, "feature", "head2", "head1")
	if err != nil {
		t.Fatal(err)
	}
	laterSR, err := f.db.InsertStepResult(laterRun.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	later := &pipeline.StepContext{DB: f.db, Repo: f.repo, Run: laterRun, StepResultID: laterSR.ID}
	pipeline.BindBranchDecisions(later)
	rounds, err := f.db.GetRoundsByStep(f.reviewSR.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior := &pipeline.StepContext{PreviousRunReviewRounds: rounds, UncertifiedPriorRounds: rounds}
	for name, output := range map[string]string{
		"own":         roundHistoryPromptSection(own),
		"other step":  runDecisionsPromptSection(f.testStepContext()),
		"later run":   branchDecisionsPromptSection(later),
		"superseded":  supersededReviewHistoryPromptSection(prior),
		"uncertified": uncertifiedRoundHistoryPromptSection(prior),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(output, "external-A") || !strings.Contains(output, "verified outside worktree") {
				t.Fatalf("external decision or reason missing from emitted prompt: %s", output)
			}
			if (name == "other step" || name == "later run") && strings.Contains(output, "undecided-B") {
				t.Fatalf("external subset included undecided finding: %s", output)
			}
			if strings.Contains(output, "declined:") || strings.Contains(output, "user_chose_to_ignore:") {
				t.Fatalf("external decision rendered as a decline: %s", output)
			}
			if name != "own" && strings.Contains(output, "private driver note") {
				t.Fatalf("driver guidance leaked into user decision: %s", output)
			}
		})
	}
}

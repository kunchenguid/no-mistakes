package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSplitFixResponse_ExternalSubsetDoesNotDecideOtherFindings(t *testing.T) {
	shown := `{"findings":[{"id":"A","description":"external fact"},{"id":"B","description":"unresolved defect"}]}`
	external := `{"findings":[{"id":"A","description":"external fact","user_instructions":"verified externally"}]}`
	gate := `{"findings":[{"id":"B","description":"unresolved defect"},{"id":"C","description":"new defect"}]}`
	source, empty := db.RoundSelectionSourceExternal, db.DeclinedSelectionJSON
	rounds := []*db.StepRound{{FindingsJSON: &shown, UserFindingsJSON: &external, SelectionSource: &source, SelectedFindingIDs: &empty}}
	if _, err := splitFixResponse(gate, rounds, []string{"C"}, nil); err == nil {
		t.Fatal("external decision for A allowed omission of undecided B")
	}
	if _, err := splitFixResponse(gate, rounds, []string{"C"}, []string{"B"}); err != nil {
		t.Fatal(err)
	}
}

func TestFilterSettledReviewFindings_UserAddedFixSurvivesLaterOmission(t *testing.T) {
	first := `{"findings":[{"id":"original","description":"original defect"}]}`
	added := `{"findings":[{"id":"A","file":"first.go","description":"requested addition"}]}`
	later := `{"findings":[{"id":"A","file":"first.go","description":"requested addition"},{"id":"B","file":"second.go","description":"new defect"}],"review_coverage_scope":{"paths":["first.go","second.go"]}}`
	user, selectA, selectB := db.RoundSelectionSourceUser, `["A"]`, `["B"]`
	rounds := []*db.StepRound{
		{FindingsJSON: &first, UserFindingsJSON: &added, SelectionSource: &user, SelectedFindingIDs: &selectA},
		{FindingsJSON: &later, SelectionSource: &user, SelectedFindingIDs: &selectB},
	}
	raw, err := filterSettledReviewFindings(later, rounds)
	if err != nil {
		t.Fatal(err)
	}
	got, err := types.ParseFindingsJSON(raw)
	if err != nil || len(got.Items) != 2 {
		t.Fatalf("earlier requested fix disappeared: %+v %v", got, err)
	}
}

func TestFilterSettledReviewFindings_ExternalSettlementOverridesEarlierFix(t *testing.T) {
	item := types.Finding{ID: "A", File: "first.go", Description: "external fact", Severity: types.FindingSeverityInfo}
	shown, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	item.UserInstructions = "verified outside worktree"
	resolved, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	user, external := db.RoundSelectionSourceUser, db.RoundSelectionSourceExternal
	selected, empty := `["A"]`, db.DeclinedSelectionJSON
	rounds := []*db.StepRound{
		{FindingsJSON: &shown, SelectionSource: &user, SelectedFindingIDs: &selected},
		{FindingsJSON: &shown, UserFindingsJSON: &resolved, SelectionSource: &external, SelectedFindingIDs: &empty},
	}
	for _, representation := range []string{"carried", "fresh"} {
		t.Run(representation, func(t *testing.T) {
			current := item
			current.UserInstructions = ""
			if representation == "fresh" {
				current.ID = "reissued-A"
				current.Line = 30
			}
			raw, err := types.MarshalFindingsJSON(types.Findings{
				Items:               []types.Finding{current},
				ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			filtered, err := filterSettledReviewFindings(raw, rounds)
			if err != nil {
				t.Fatal(err)
			}
			got, err := types.ParseFindingsJSON(filtered)
			if err != nil || len(got.Items) != 0 {
				t.Fatalf("acknowledged external settlement remained blocked: %+v %v", got, err)
			}
		})
	}
}

func TestExecutor_ExternalSettlementBindsNewFixerProvenance(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	fromSHA := run.HeadSHA
	var toSHA string
	calls := 0
	item := types.Finding{ID: "A", File: "first.go", Description: "external fact", Severity: types.FindingSeverityInfo, Action: types.ActionNoOp}
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		switch calls {
		case 1:
			if sctx.UncertifiedToSHA != "" {
				t.Error("initial review unexpectedly has fixer provenance")
			}
		case 2:
			if !sctx.Fixing {
				t.Error("requested fix did not run in fix mode")
			}
			if _, err := git.Run(sctx.Ctx, workDir, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixer change"); err != nil {
				return nil, err
			}
			var err error
			toSHA, err = git.HeadSHA(sctx.Ctx, workDir)
			if err != nil {
				return nil, err
			}
			if err := database.UpdateRunHeadSHA(run.ID, toSHA); err != nil {
				return nil, err
			}
			sctx.Run.HeadSHA = toSHA
			PersistUncertifiedPipelineRange(sctx, fromSHA, toSHA)
		case 3:
			if sctx.Fixing || sctx.SkipFixExecution || sctx.FinalizingAnswers || sctx.CoverageContinuation != nil {
				t.Error("external settlement did not return to static review")
			}
			if sctx.UncertifiedFromSHA != fromSHA || sctx.UncertifiedToSHA != toSHA || sctx.UncertifiedSourceRunID != run.ID {
				t.Errorf("static rereview lost fixer provenance: from=%q to=%q source=%q", sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, sctx.UncertifiedSourceRunID)
			}
		default:
			t.Errorf("unexpected review turn %d", calls)
		}
		return dispositionCoverageOutcome(t, sctx, []types.Finding{item}, []string{"first.go", "second.go"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- exec.Execute(ctx, run, repo, workDir)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("read parked review: %+v %v", steps, err)
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"A"}, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	waitForRounds(t, database, steps[0].ID, 2, types.StepStatusFixReview)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"A"}, nil, nil, nil, "verified outside worktree"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("external settlement did not complete")
	}
	if calls != 3 {
		t.Fatalf("review turns = %d, want 3", calls)
	}
}

func TestExecutor_RecoveredExternalDecisionReplaysWithoutReplacement(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(sr.ID); err != nil {
		t.Fatal(err)
	}
	item := types.Finding{ID: "A", File: "first.go", Description: "external fact", Severity: types.FindingSeverityInfo, Action: types.ActionNoOp}
	outcome := dispositionCoverageOutcome(t, &StepContext{Run: run, StepResultID: sr.ID}, []types.Finding{item}, []string{"first.go", "second.go"})
	if err := database.SetStepFindings(sr.ID, outcome.Findings); err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(sr.ID, 1, "initial", &outcome.Findings, nil, 25)
	if err != nil {
		t.Fatal(err)
	}
	item.UserInstructions = "verified outside worktree"
	recorded, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	if err != nil {
		t.Fatal(err)
	}
	empty := db.DeclinedSelectionJSON
	if err := database.SetStepRoundUserDecision(round.ID, &empty, db.RoundSelectionSourceExternal, &recorded, types.ResponseOriginUnknown); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(sr.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertUncertifiedPipelineRange(repo.ID, run.Branch, run.BaseSHA, run.HeadSHA, run.ID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if sctx.Fixing {
			t.Error("external replay ran a fixer")
		}
		if sctx.UncertifiedFromSHA != run.BaseSHA || sctx.UncertifiedToSHA != run.HeadSHA || sctx.UncertifiedSourceRunID != run.ID {
			t.Errorf("external replay lost fixer provenance: from=%q to=%q source=%q", sctx.UncertifiedFromSHA, sctx.UncertifiedToSHA, sctx.UncertifiedSourceRunID)
		}
		return dispositionCoverageOutcome(t, sctx, []types.Finding{item}, []string{"first.go", "second.go"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- exec.Resume(ctx, run, repo, workDir)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	deadline := time.Now().Add(5 * time.Second)
	for {
		exec.mu.Lock()
		waiting := exec.waiting
		exec.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate did not park")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"A"}, nil, nil, nil, ""); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("fix replaced accepted external decision: %v", err)
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"A"}, nil, nil, nil, "changed reason"); err == nil {
		t.Fatal("replay replaced reason")
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"A"}, nil, nil, nil, item.UserInstructions); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("external replay did not complete")
	}
	rounds, err := database.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(rounds) != 2 || rounds[1].Trigger != "external" {
		t.Fatalf("replay did not run one static review: calls=%d rounds=%+v", calls, rounds)
	}
	if rounds[0].UserFindingsJSON == nil || *rounds[0].UserFindingsJSON != recorded {
		t.Fatal("replay changed durable decision")
	}
}

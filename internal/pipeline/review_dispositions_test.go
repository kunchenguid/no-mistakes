package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func dispositionCoverageOutcome(t *testing.T, sctx *StepContext, items []types.Finding, reviewed []string) *StepOutcome {
	t.Helper()
	var findings types.Findings
	if err := json.Unmarshal([]byte(coverageFixtureFindings(t, sctx.Run, sctx.StepResultID, reviewed, []string{"first.go", "second.go"})), &findings); err != nil {
		t.Fatal(err)
	}
	findings.Items = items
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatal(err)
	}
	return &StepOutcome{NeedsApproval: len(items) != 0 || len(reviewed) != 2, Findings: encoded, AutoFixable: len(items) != 0, ReviewedPaths: reviewed, ReviewablePaths: []string{"first.go", "second.go"}, ReviewApprovedHeadSHA: sctx.Run.HeadSHA}
}

func TestExecutor_SettledFindingDoesNotBlockCoverageContinuation(t *testing.T) {
	for _, mode := range []string{"ignore", "resolved_external"} {
		t.Run(mode, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := coverageWorkdir(t, database, run)
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				if calls == 4 {
					if sctx.Fixing {
						t.Error("coverage continuation ran a fix")
					}
					return dispositionCoverageOutcome(t, sctx, nil, []string{"first.go", "second.go"}), nil
				}
				id := "original"
				line := 12
				if calls == 2 {
					id = "reissued"
					line = 19
				} else if calls == 3 {
					id = "coverage-reissued"
					line = 27
				}
				return dispositionCoverageOutcome(t, sctx, []types.Finding{{ID: id, File: "first.go", Line: line, Severity: types.FindingSeverityInfo, Description: "private maintainer grant already settled", Action: types.ActionNoOp}}, []string{"first.go"}), nil
			}}
			exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			done := make(chan error, 1)
			go func() { defer close(finished); done <- exec.Execute(ctx, run, repo, workDir) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("executor did not exit")
				}
			})
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			var err error
			if mode == "ignore" {
				_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, nil, []string{"original"}, nil, nil, "")
			} else {
				_, err = exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"original"}, nil, nil, nil, "private grant verified on the machine")
			}
			if err != nil {
				t.Fatal(err)
			}
			status := types.StepStatusAwaitingApproval
			if mode == "ignore" {
				status = types.StepStatusFixReview
			}
			waitForStepStatus(t, database, run.ID, types.StepReview, status)
			deadline := time.Now().Add(5 * time.Second)
			for attempt := 0; attempt < 2; attempt++ {
				for {
					binding := coverageBinding(t, database, run.ID)
					_, err = exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: binding})
					if err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("settled finding blocked coverage round %d: %v", attempt+1, err)
					}
					time.Sleep(5 * time.Millisecond)
				}
				if attempt == 0 {
					waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("coverage did not finish")
			}
			if calls != 4 {
				t.Fatalf("review turns = %d, want 4", calls)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || steps[0].Status != types.StepStatusCompleted {
				t.Fatalf("review did not complete: %+v %v", steps, err)
			}
			if mode == "resolved_external" {
				rounds, err := database.GetRoundsByStep(steps[0].ID)
				if err != nil || len(rounds) < 2 || rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceExternal || rounds[0].UserFindingsJSON == nil {
					t.Fatalf("external decision was not durable: %+v %v", rounds, err)
				}
				resolved, err := types.ParseFindingsJSON(*rounds[0].UserFindingsJSON)
				if err != nil || len(resolved.Items) != 1 || resolved.Items[0].UserInstructions != "private grant verified on the machine" {
					t.Fatalf("external reason was lost: %+v %v", resolved, err)
				}
				if rounds[1].Trigger != "external" {
					t.Fatalf("second round trigger = %q, want external", rounds[1].Trigger)
				}
			}
		})
	}
}

func TestExecutor_ExternalDispositionRequiresReasonAndKeepsOtherFindingsBlocking(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		return dispositionCoverageOutcome(t, sctx, []types.Finding{
			{ID: "settled", File: "first.go", Severity: types.FindingSeverityInfo, Description: "private grant", Action: types.ActionNoOp},
			{ID: "error", File: "second.go", Severity: types.FindingSeverityError, Description: "live defect", Action: types.ActionNoOp},
			{ID: "warning", File: "second.go", Severity: types.FindingSeverityWarning, Description: "live concern", Action: types.ActionAskUser},
		}, []string{"first.go"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); _ = exec.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() { cancel(); <-finished })
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"settled"}, nil, nil, nil, " "); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("missing reason was accepted: %v", err)
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionResolvedExternal, []string{"settled"}, nil, nil, nil, "verified outside review worktree"); err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: coverageBinding(t, database, run.ID)}); err == nil || !strings.Contains(err.Error(), "outstanding") {
		t.Fatalf("unsettled error/warning did not block coverage: %v", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFilterSettledReviewFindings_LegacyIgnoreOnlyFixIsStickyButApprovalIsNot(t *testing.T) {
	item := types.Finding{ID: "original", File: "first.go", Severity: types.FindingSeverityInfo, Description: "grant settled", Action: types.ActionNoOp}
	prior, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	item.ID = "new-id"
	item.Line = 31
	current := types.Findings{Items: []types.Finding{item}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}}
	raw, _ := types.MarshalFindingsJSON(current)
	selection := db.DeclinedSelectionJSON
	source := db.RoundSelectionSourceUserDeclined
	rounds := []*db.StepRound{{FindingsJSON: &prior, SelectionSource: &source, SelectedFindingIDs: &selection}}
	for _, tc := range []struct {
		name, trigger string
		want          int
	}{{"approval", "coverage", 1}, {"legacy ignore", "auto_fix", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			later := append(append([]*db.StepRound(nil), rounds...), &db.StepRound{Trigger: tc.trigger})
			filtered, err := filterSettledReviewFindings(raw, later)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := types.ParseFindingsJSON(filtered)
			if err != nil || len(parsed.Items) != tc.want {
				t.Fatalf("items = %d, want %d: %v", len(parsed.Items), tc.want, err)
			}
		})
	}
}

func TestFilterSettledReviewFindings_AmbiguousFingerprintFailsClosed(t *testing.T) {
	item := types.Finding{ID: "first", File: "first.go", Severity: types.FindingSeverityInfo, Description: "same", Action: types.ActionNoOp}
	prior, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	selection := db.DeclinedSelectionJSON
	source := db.RoundSelectionSourceUserIgnored
	rounds := []*db.StepRound{{FindingsJSON: &prior, SelectionSource: &source, SelectedFindingIDs: &selection}}
	current := types.Findings{Items: []types.Finding{item, item}, ReviewCoverageScope: &types.ReviewCoverageScope{Paths: []string{"first.go", "second.go"}}}
	raw, _ := types.MarshalFindingsJSON(current)
	filtered, err := filterSettledReviewFindings(raw, rounds)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := types.ParseFindingsJSON(filtered)
	if err != nil || len(parsed.Items) != 2 {
		t.Fatalf("ambiguous findings were dropped: %+v %v", parsed, err)
	}
}

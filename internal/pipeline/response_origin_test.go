package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ResponseOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin types.ResponseOrigin
		want   string
	}{
		{types.ResponseOriginAutoYes, "auto_yes"},
		{types.ResponseOriginInteractive, "interactive"},
		{"", "unknown"},
	} {
		for _, action := range []types.ApprovalAction{types.ActionFix, types.ActionApprove, types.ActionSkip, types.ActionAbort} {
			t.Run(string(tc.origin)+"/"+string(action), func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				parked := make(chan struct{}, 1)
				step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
					if sctx.Fixing {
						return &StepOutcome{ReviewedPaths: []string{"x.js"}, ReviewablePaths: []string{"x.js"}}, nil
					}
					return &StepOutcome{NeedsApproval: true, Findings: declinedTestFindings}, nil
				}}
				exec := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
					if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
						parked <- struct{}{}
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				done := make(chan error, 1)
				workDir := t.TempDir()
				initGitRepo(t, workDir)
				head, err := git.HeadSHA(ctx, workDir)
				if err != nil {
					t.Fatal(err)
				}
				run.HeadSHA = head
				if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
					t.Fatal(err)
				}
				go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
				select {
				case <-parked:
				case <-ctx.Done():
					t.Fatal("gate did not park")
				}
				var ids []string
				if action == types.ActionFix {
					ids = []string{"journal-version-deduplication"}
				}
				options := []ResponseOptions(nil)
				if tc.origin != "" {
					options = append(options, ResponseOptions{Origin: tc.origin})
				}
				if _, err := exec.RespondWithOverrides(types.StepReview, action, ids, nil, nil, nil, "", options...); err != nil {
					t.Fatal(err)
				}
				// The response must be durable before dispatch acknowledgement, not only
				// after the executor finishes its next round.
				steps, err := database.GetStepsByRun(run.ID)
				if err != nil || len(steps) != 1 {
					t.Fatalf("steps: %v %v", steps, err)
				}
				if action == types.ActionFix {
					rounds, err := database.GetRoundsByStep(steps[0].ID)
					if err != nil || len(rounds) == 0 {
						t.Fatalf("rounds: %v %v", rounds, err)
					}
					if string(rounds[0].ResponseOrigin) != tc.want {
						t.Fatalf("round origin = %q, want %q", rounds[0].ResponseOrigin, tc.want)
					}
					if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != "user" {
						t.Fatalf("selection semantics changed: %+v", rounds[0])
					}
				} else if string(steps[0].ResponseOrigin) != tc.want {
					t.Fatalf("step origin = %q, want %q", steps[0].ResponseOrigin, tc.want)
				}
				select {
				case err := <-done:
					if action != types.ActionAbort && err != nil {
						t.Fatal(err)
					}
					final, err := database.GetStepResult(steps[0].ID)
					if err != nil {
						t.Fatal(err)
					}
					want := types.StepStatusCompleted
					if action == types.ActionSkip {
						want = types.StepStatusSkipped
					}
					if action == types.ActionAbort {
						want = types.StepStatusFailed
					}
					if final.Status != want {
						t.Fatalf("origin changed gate outcome: got %s, want %s", final.Status, want)
					}
				case <-ctx.Done():
					t.Fatal("executor did not finish")
				}
			})
		}
	}
}

package daemon

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The draft-until-ready policy belongs to the branch, not to the one trigger
// that happened to carry the flag. A rerun, a TUI rerun, or a plain gate push
// with no push option would otherwise adopt the draft PR an earlier run opened
// and never publish it, leaving reviewers untagged forever.
func TestStartRunCarriesDraftUntilReadyForwardOnTheSameBranch(t *testing.T) {
	tests := []struct {
		name          string
		priorPolicies []bool
		priorBranch   string
		requested     bool
		want          bool
	}{
		{name: "no prior run and no request stays off", want: false},
		{name: "explicit request turns it on", requested: true, want: true},
		{name: "inherits from the branch's last run", priorPolicies: []bool{true}, want: true},
		{name: "explicit request still wins over an off prior run", priorPolicies: []bool{false}, requested: true, want: true},
		{name: "an ordinary branch never inherits it", priorPolicies: []bool{false}, want: false},
		{name: "the most recent run on the branch is authoritative", priorPolicies: []bool{false, true}, want: true},
		{name: "another branch's policy never leaks in", priorPolicies: []bool{true}, priorBranch: "other", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NM_DEMO", "1")
			p, database := newRefreshRunFixture(t)
			repo, head := setupTestGitRepo(t, p, database, "draft-carry-forward")

			priorBranch := tt.priorBranch
			if priorBranch == "" {
				priorBranch = "main"
			}
			for _, policy := range tt.priorPolicies {
				prior, err := database.InsertRun(repo.ID, priorBranch, head, refreshTestZeroSHA)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.SetRunDraftUntilReady(prior.ID, policy); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunStatus(prior.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
			}

			seen := make(chan bool, 1)
			manager := NewRunManager(database, p, func() []pipeline.Step {
				return []pipeline.Step{&captureDraftPolicyStep{seen: seen}}
			})
			t.Cleanup(manager.Shutdown)

			runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", RunOptions{DraftUntilReady: tt.requested})
			if err != nil {
				t.Fatalf("start run: %v", err)
			}
			if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
				t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
			}

			if got := <-seen; got != tt.want {
				t.Fatalf("pipeline saw DraftUntilReady = %v, want %v", got, tt.want)
			}
			stored, err := database.GetRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.DraftUntilReady != tt.want {
				t.Fatalf("runs.draft_until_ready = %v, want %v", stored.DraftUntilReady, tt.want)
			}
		})
	}
}

type captureDraftPolicyStep struct {
	seen chan<- bool
}

func (s *captureDraftPolicyStep) Name() types.StepName { return types.StepReview }
func (s *captureDraftPolicyStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	var policy bool
	if sctx.Run != nil {
		policy = sctx.Run.DraftUntilReady
	}
	s.seen <- policy
	return &pipeline.StepOutcome{}, nil
}

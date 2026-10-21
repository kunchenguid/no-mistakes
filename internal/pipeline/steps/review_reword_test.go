package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rewordFixRound builds a run at its post-rebase head (a replay onto a moved
// base when rebase is set), resolves its Rebase row with resolve the way the
// executor does (none when empty), records four Review fixer commits on top
// of an uncertified range an earlier run left at base, then hands off a fixer
// turn that rewords those four commits with every tree unchanged.
func rewordFixRound(t *testing.T, rebase bool, resolve types.StepStatus) (sctx *pipeline.StepContext, base, recorded, rewritten string, err error) {
	t.Helper()
	dir, base, submitted := setupGitRepo(t)
	sctx = newTestContextWithDBRecords(t, &mockAgent{name: "codex"}, dir, base, submitted, config.Commands{})
	sctx.Fixing = true
	rebased := submitted
	gitCmd(t, dir, "checkout", "--detach", submitted)
	if rebase {
		gitCmd(t, dir, "checkout", "--detach", base)
		if err := os.WriteFile(filepath.Join(dir, "upstream"), []byte("moved base"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "upstream")
		gitCmd(t, dir, "commit", "-m", "upstream moved")
		gitCmd(t, dir, "cherry-pick", base+".."+submitted)
		rebased = gitCmd(t, dir, "rev-parse", "HEAD")
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, rebased); err != nil {
			t.Fatal(err)
		}
		sctx.Run.HeadSHA = rebased
	}
	if resolve != "" {
		sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepRebase)
		if err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.CompleteStepWithStatus(sr.ID, resolve, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := sctx.DB.UpsertUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch, base, rebased, "earlier-run"); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("fix-%d", i)), []byte(fmt.Sprintf("fix %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := commitAgentFixes(sctx, types.StepReview, fmt.Sprintf("fix %d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	recorded = sctx.Run.HeadSHA
	var summary string
	summary, err = executeFixMode(sctx, types.StepReview, fixExecutionOptions{
		RunAgent: func(agent.RunOpts) (*agent.Result, error) {
			parent := rebased
			for i, head := range strings.Fields(gitCmd(t, dir, "rev-list", "--reverse", rebased+".."+recorded)) {
				tree := gitCmd(t, dir, "rev-parse", head+"^{tree}")
				parent = gitCmd(t, dir, "commit-tree", tree, "-p", parent, "-m", fmt.Sprintf("fix: reword %d", i))
			}
			rewritten = parent
			gitCmd(t, dir, "checkout", "--detach", rewritten)
			return &agent.Result{Output: json.RawMessage(`{"summary":"reword fix commits"}`)}, nil
		},
	})
	if err == nil && summary != changesAppliedSummary {
		t.Fatalf("fix handoff summary = %q", summary)
	}
	return sctx, base, recorded, rewritten, err
}

func TestReviewFixRewordsFourCommitsBeforeIndependentReview(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rebase  bool
		resolve types.StepStatus
	}{
		{"rebase-moved-head", true, types.StepStatusCompleted},
		{"rebase-skipped", false, types.StepStatusSkipped},
		{"conflict-approved", false, types.StepStatusCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, base, recorded, rewritten, err := rewordFixRound(t, tc.rebase, tc.resolve)
			if err != nil {
				t.Fatalf("fix handoff: %v", err)
			}
			dir := sctx.WorkDir
			if rewritten == recorded || sctx.Run.HeadSHA != rewritten {
				t.Fatal("rewrite was not recorded")
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || run.HeadSHA != rewritten || run.ReviewApprovedHeadSHA != nil {
				t.Fatalf("rewrite incorrectly approved: %+v %v", run, err)
			}
			for _, head := range []string{recorded, rewritten} {
				if gitCmd(t, dir, "rev-parse", custody.RewordRef(run.ID, head)) != head {
					t.Fatal("history not anchored")
				}
			}
			if err := assertPipelineHeadContinuity(sctx, types.StepReview); err != nil {
				t.Fatalf("rereview entry: %v", err)
			}
			if gitCmd(t, dir, "rev-parse", recorded+"^{tree}") != gitCmd(t, dir, "rev-parse", rewritten+"^{tree}") {
				t.Fatal("lost fixes")
			}
			rng, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
			if err != nil || rng == nil || rng.FromSHA != base || rng.ToSHA != rewritten {
				t.Fatalf("uncertified range = %+v %v, want %s..%s", rng, err, base, rewritten)
			}
		})
	}
}

func TestReviewFixRewordRefusesARunWithoutARecordedPostRebaseHead(t *testing.T) {
	t.Parallel()
	sctx, _, recorded, _, err := rewordFixRound(t, true, "")
	if err == nil || !strings.Contains(err.Error(), "post-rebase head") {
		t.Fatalf("err = %v, want post-rebase head refusal", err)
	}
	run, dbErr := sctx.DB.GetRun(sctx.Run.ID)
	if dbErr != nil || run.HeadSHA != recorded || sctx.Run.HeadSHA != recorded {
		t.Fatalf("refusal moved the recorded head: %+v %v", run, dbErr)
	}
}

func TestReviewFixRefusesRewriteAlreadyPresentBeforeAgentInvocation(t *testing.T) {
	t.Parallel()
	dir, base, submitted := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "codex"}, dir, base, submitted, config.Commands{})
	sctx.Fixing = true
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "fix")
	if err := recordAgentFixHead(sctx, types.StepReview, gitCmd(t, dir, "rev-parse", "HEAD")); err != nil {
		t.Fatal(err)
	}
	recorded := sctx.Run.HeadSHA
	gitCmd(t, dir, "commit", "--amend", "--allow-empty", "-m", "out of band reword")
	called := false
	_, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{RunAgent: func(agent.RunOpts) (*agent.Result, error) { called = true; return nil, nil }})
	if err == nil || called || sctx.Run.HeadSHA != recorded {
		t.Fatalf("entry guard bypassed: called=%v err=%v", called, err)
	}
}

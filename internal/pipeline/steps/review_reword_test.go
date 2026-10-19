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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewFixRewordsFourCommitsBeforeIndependentReview(t *testing.T) {
	t.Parallel()
	dir, base, submitted := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "codex"}, dir, base, submitted, config.Commands{})
	sctx.Fixing = true
	gitCmd(t, dir, "checkout", "--detach", submitted)
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("fix-%d", i)), []byte(fmt.Sprintf("fix %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := commitAgentFixes(sctx, types.StepReview, fmt.Sprintf("fix %d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	recorded := sctx.Run.HeadSHA
	rewritten := ""
	summary, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{
		RunAgent: func(agent.RunOpts) (*agent.Result, error) {
			parent := submitted
			for i, head := range strings.Fields(gitCmd(t, dir, "rev-list", "--reverse", submitted+".."+recorded)) {
				tree := gitCmd(t, dir, "rev-parse", head+"^{tree}")
				parent = gitCmd(t, dir, "commit-tree", tree, "-p", parent, "-m", fmt.Sprintf("fix: reword %d", i))
			}
			rewritten = parent
			gitCmd(t, dir, "checkout", "--detach", rewritten)
			return &agent.Result{Output: json.RawMessage(`{"summary":"reword fix commits"}`)}, nil
		},
	})
	if err != nil || summary != changesAppliedSummary {
		t.Fatalf("fix handoff: %s %v", summary, err)
	}
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

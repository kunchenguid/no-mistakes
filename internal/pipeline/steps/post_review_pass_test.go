package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// postReviewPushFixture is a run whose feature head Review approved and the
// remote already carries, ready for a later step to commit after it.
func postReviewPushFixture(t *testing.T) (sctx *pipeline.StepContext, dir, upstream, approvedHead string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	sctx = newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	// Keep this fixture's gate mirror local: setupGateMirror changes NM_HOME
	// process-wide and prevents these independent git-backed cases from running
	// in parallel on the process-spawn-bound Windows steps shard.
	gateDir := paths.WithRoot(t.TempDir()).RepoDir(sctx.Repo.ID)
	sctx.GateDir = gateDir
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, filepath.Dir(gateDir), "init", "--bare", filepath.Base(gateDir))
	recordReviewApproval(t, sctx, headSHA)
	return sctx, dir, upstream, headSHA
}

// commitAfterReview records a commit the way a later pipeline step does.
func commitAfterReview(t *testing.T, sctx *pipeline.StepContext, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", file)
	gitCmd(t, dir, "commit", "-m", "no-mistakes(document): update "+file)
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = head
	return head
}

func TestPushStep_PostReviewPassStopsUnreviewedCommitsUntilReviewed(t *testing.T) {
	t.Parallel()
	sctx, dir, upstream, approvedHead := postReviewPushFixture(t)
	sctx.Config.Review.PostReviewPass = true
	documentHead := commitAfterReview(t, sctx, dir, "README.md", "# Feature\n")

	outcome, err := (&PushStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.PostReviewPass {
		t.Fatal("push published a commit Review never saw; want a post-review pass request")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != approvedHead {
		t.Fatalf("remote head = %s, want the reviewed head %s untouched", got, approvedHead)
	}

	// The pass certified the new head: Push now publishes it.
	recordReviewApproval(t, sctx, documentHead)
	outcome, err = (&PushStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PostReviewPass {
		t.Fatal("push asked for another pass over a head Review approved")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != documentHead {
		t.Fatalf("remote head = %s, want %s", got, documentHead)
	}
}

// Push's own leftover commit is a commit after Review too, and it must be
// recorded before the pass so the pass's range reaches it.
func TestPushStep_PostReviewPassCoversPushsOwnLeftoverCommit(t *testing.T) {
	t.Parallel()
	sctx, dir, upstream, approvedHead := postReviewPushFixture(t)
	sctx.Config.Review.PostReviewPass = true
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("left behind by an agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&PushStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.PostReviewPass {
		t.Fatal("push published its own unreviewed leftover commit")
	}
	committed := gitCmd(t, dir, "rev-parse", "HEAD")
	if committed == approvedHead {
		t.Fatal("expected Push to commit the leftover change")
	}
	if sctx.Run.HeadSHA != committed {
		t.Fatalf("run head = %s, want the leftover commit %s recorded for the pass", sctx.Run.HeadSHA, committed)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != committed {
		t.Fatalf("durable run head = %s, want %s", run.HeadSHA, committed)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != approvedHead {
		t.Fatalf("remote head = %s, want the reviewed head %s untouched", got, approvedHead)
	}
}

func TestPushStep_PostReviewPassPublishesWhenNotRequired(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, sctx *pipeline.StepContext)
	}{
		{"setting off", func(*testing.T, *pipeline.StepContext) {}},
		{"review skipped at its gate", func(t *testing.T, sctx *pipeline.StepContext) {
			sctx.Config.Review.PostReviewPass = true
			review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.CompleteStepWithStatus(review.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, dir, upstream, _ := postReviewPushFixture(t)
			tc.setup(t, sctx)
			documentHead := commitAfterReview(t, sctx, dir, "README.md", "# Feature\n")

			outcome, err := (&PushStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.PostReviewPass {
				t.Fatal("push asked for a post-review pass it does not owe")
			}
			if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != documentHead {
				t.Fatalf("remote head = %s, want %s", got, documentHead)
			}
		})
	}
}

// The pass reviews only what landed after the approved head: its base commit
// is that head, its coverage contract lists only the files those commits
// changed, and the reviewer is told the commits are pipeline-authored.
func TestReviewStep_PostReviewPassReviewsOnlyCommitsAfterTheApprovedHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, approvedHead := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "README.md")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(document): describe the feature")
	documentHead := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := newStaticReviewAgent(coverageFindingJSON([]string{"README.md"}))
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, documentHead, config.Commands{})
	sctx.PostReviewPassFrom = approvedHead

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.ReviewApprovedHeadSHA != documentHead {
		t.Fatalf("approved head = %q, want the head the pass reviewed %s", outcome.ReviewApprovedHeadSHA, documentHead)
	}
	if strings.Join(outcome.ReviewablePaths, ",") != "README.md" {
		t.Fatalf("reviewable paths = %q, want only the file committed after Review", outcome.ReviewablePaths)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{"- base commit: " + approvedHead + "\n", "Post-review pass:", "- README.md\n"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("pass prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "- feature.txt\n") {
		t.Fatal("pass coverage contract lists a file Review already approved")
	}

	// An ordinary review is unchanged: it never sees the pass's framing.
	ordinary := newStaticReviewAgent(coverageFindingJSON([]string{"README.md", "feature.txt"}))
	sctx = newTestContextWithDBRecords(t, ordinary, dir, baseSHA, documentHead, config.Commands{})
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(ordinary.calls[0].Prompt, "Post-review pass:") {
		t.Fatal("an ordinary review carries the post-review pass framing")
	}
}

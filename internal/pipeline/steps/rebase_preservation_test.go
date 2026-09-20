package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

type submittedMergeFixture struct {
	dir       string
	upstream  string
	baseSHA   string
	submitted string
	private   string
}

// newSubmittedMergeFixture models the handoff that can happen before a run:
// the submitted head is a deliberate merge whose second parent is the private
// mirror head, while the upstream base later moves and conflicts with the
// corrected first-parent history.
func newSubmittedMergeFixture(t *testing.T) submittedMergeFixture {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFixtureFile(t, dir, "shared.txt", "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFixtureFile(t, dir, "shared.txt", "corrected\n")
	gitCmd(t, dir, "commit", "-am", "corrected first parent")

	gitCmd(t, dir, "checkout", "-b", "private", baseSHA)
	writeFixtureFile(t, dir, "private.txt", "private mirror content\n")
	gitCmd(t, dir, "add", "private.txt")
	gitCmd(t, dir, "commit", "-m", "private mirror content")
	private := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "merge", "--no-ff", "--no-edit", "private")
	submitted := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "checkout", "main")
	writeFixtureFile(t, dir, "shared.txt", "upstream\n")
	gitCmd(t, dir, "commit", "-am", "upstream moved base")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	return submittedMergeFixture{dir: dir, upstream: upstream, baseSHA: baseSHA, submitted: submitted, private: private}
}

func TestRebaseStep_PreservesSubmittedPrivateMergeThroughRebase(t *testing.T) {
	f := newSubmittedMergeFixture(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.submitted, config.Commands{})
	sctx.Repo.UpstreamURL = f.upstream
	gateDir := setupGateMirror(t, sctx)
	gitCmd(t, gateDir, "fetch", f.dir, f.private+":refs/heads/feature")

	var resolverCalls int
	sctx.Agent = &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			resolverCalls++
			writeFixtureFile(t, f.dir, "shared.txt", "corrected\n")
			fixtureGit(t, f.dir, "add", "shared.txt")
			fixtureGit(t, f.dir, "rebase", "--continue")
			return &agent.Result{Output: json.RawMessage(`{"summary":"kept corrected history"}`)}, nil
		},
	}

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the initial conflict gate, got %#v", outcome)
	}

	sctx.Fixing = true
	outcome, err = (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("expected the repaired rebase to complete, got %#v", outcome)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}

	finalHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	finalParents := parents(t, f.dir, finalHead)
	if len(finalParents) != 2 || finalParents[1] != f.submitted {
		t.Fatalf("final head %s parents = %v, want [rebased-head %s]", finalHead, finalParents, f.submitted)
	}
	if finalParents[0] == f.submitted {
		t.Fatalf("final topology did not retain the rebased head as first parent: %v", finalParents)
	}
	if !isAncestor(context.Background(), f.dir, f.private, finalHead) {
		t.Fatalf("private mirror %s is not an ancestor of final head %s", f.private, finalHead)
	}
	if got := gitCmd(t, f.dir, "rev-parse", finalHead+"^{tree}"); got != gitCmd(t, f.dir, "rev-parse", finalParents[0]+"^{tree}") {
		t.Fatalf("topology bridge changed the accepted tree")
	}
	if got := gitCmd(t, f.dir, "show", finalHead+":shared.txt"); got != "corrected" {
		t.Fatalf("accepted rebase tree was replaced: shared.txt = %q", got)
	}
	if got := gitCmd(t, f.dir, "show", finalHead+":private.txt"); got != "private mirror content" {
		t.Fatalf("private history content was lost: private.txt = %q", got)
	}
	if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != f.private {
		t.Fatalf("gate mirror moved during rebase to %s, want %s", got, f.private)
	}
	if sctx.Run.HeadSHA != finalHead {
		t.Fatalf("run head = %s, want final topology head %s", sctx.Run.HeadSHA, finalHead)
	}
	persisted, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.HeadSHA != finalHead {
		t.Fatalf("persisted run head = %v, want final topology head %s", persisted, finalHead)
	}

	// The final topology head contains the prior branch head through the
	// submitted merge, so publication must be append-only and must advance the
	// private mirror without archiving or deleting it.
	recordReviewApproval(t, sctx, finalHead)
	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("publish preserved merge head: %v", err)
	}
	if got := gitCmd(t, f.upstream, "rev-parse", "refs/heads/feature"); got != finalHead {
		t.Fatalf("published remote head = %s, want %s", got, finalHead)
	}
	if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != finalHead {
		t.Fatalf("published gate head = %s, want %s", got, finalHead)
	}
	if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("append-only publication archived the private mirror: %s", tags)
	}
}

func TestRebaseStep_DoesNotBridgeWhenPrivateMirrorDiffers(t *testing.T) {
	f := newSubmittedMergeFixture(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.submitted, config.Commands{})
	sctx.Repo.UpstreamURL = f.upstream
	gateDir := setupGateMirror(t, sctx)
	gitCmd(t, f.dir, "checkout", "private")
	writeFixtureFile(t, f.dir, "missing.txt", "genuinely private\n")
	gitCmd(t, f.dir, "add", "missing.txt")
	gitCmd(t, f.dir, "commit", "-m", "new private content")
	missingPrivate := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "checkout", "feature")
	gitCmd(t, gateDir, "fetch", f.dir, missingPrivate+":refs/heads/feature")

	sctx.Agent = &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			writeFixtureFile(t, f.dir, "shared.txt", "corrected\n")
			fixtureGit(t, f.dir, "add", "shared.txt")
			fixtureGit(t, f.dir, "rebase", "--continue")
			return &agent.Result{Output: json.RawMessage(`{"summary":"kept corrected history"}`)}, nil
		},
	}

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the initial conflict gate, got %#v", outcome)
	}
	sctx.Fixing = true
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	finalHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if len(parents(t, f.dir, finalHead)) != 1 {
		t.Fatalf("mismatched private mirror unexpectedly received a topology bridge: %s", finalHead)
	}

	recordReviewApproval(t, sctx, finalHead)
	_, err = (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected publication to refuse genuinely private mirror content")
	}
	for _, want := range []string{missingPrivate, "new private content", "at-risk"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("publication refusal = %v, want %q", err, want)
		}
	}
	if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != missingPrivate {
		t.Fatalf("refused publication moved private mirror to %s, want %s", got, missingPrivate)
	}
}

func TestPreserveSubmittedMergeParentRefusesDroppedPrivateContent(t *testing.T) {
	f := newSubmittedMergeFixture(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.submitted, config.Commands{})
	gateDir := setupGateMirror(t, sctx)
	gitCmd(t, gateDir, "fetch", f.dir, f.private+":refs/heads/feature")

	gitCmd(t, f.dir, "reset", "--hard", "origin/main")
	writeFixtureFile(t, f.dir, "shared.txt", "resolved without private content\n")
	gitCmd(t, f.dir, "commit", "-am", "rebased conflict resolution")
	rebasedHead := gitCmd(t, f.dir, "rev-parse", "HEAD")

	err := preserveSubmittedMergeParent(context.Background(), sctx, submittedMergePreservation{
		submittedHead: f.submitted,
		privateHead:   f.private,
		branchRef:     "refs/heads/feature",
	})
	if err == nil {
		t.Fatal("topology bridge accepted a rebased head that dropped private content")
	}
	for _, want := range []string{f.private, "private mirror content", "at-risk"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("preservation refusal = %v, want %q", err, want)
		}
	}
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != rebasedHead {
		t.Fatalf("refused preservation moved HEAD to %s, want %s", got, rebasedHead)
	}
	if got := parents(t, f.dir, rebasedHead); len(got) != 1 {
		t.Fatalf("refused preservation created a topology bridge: %v", got)
	}
	if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != f.private {
		t.Fatalf("refused preservation moved private mirror to %s, want %s", got, f.private)
	}
}

func TestValidateSubmittedMergeParentCancellationRestoresRebasedHead(t *testing.T) {
	f := newSubmittedMergeFixture(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.submitted, config.Commands{})

	gitCmd(t, f.dir, "reset", "--hard", "origin/main")
	currentHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	currentTree := gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}")
	fixtureGitAllowFail(t, f.dir, "merge", "--no-ff", "--no-commit", f.submitted)
	fixtureGit(t, f.dir, "read-tree", "--reset", "-u", currentHead)
	fixtureGit(t, f.dir, "commit", "--no-edit")
	if topologyHead := gitCmd(t, f.dir, "rev-parse", "HEAD"); topologyHead == currentHead {
		t.Fatal("fixture did not create a topology commit")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := validateSubmittedMergeParent(ctx, sctx, submittedMergePreservation{submittedHead: f.submitted}, currentHead, currentTree)
	if err == nil {
		t.Fatal("canceled topology validation unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "resolve topology head") {
		t.Fatalf("topology validation error = %v, want resolution failure", err)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != currentHead {
		t.Fatalf("canceled topology validation left HEAD at %s, want %s", got, currentHead)
	}
	if got := gitStatusPorcelain(t, f.dir); got != "" {
		t.Fatalf("canceled topology validation left a dirty worktree: %s", got)
	}
}

func TestRebaseStep_OrdinaryRunWithoutPrivateMergeIsUnchanged(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, config.RebaseStrategyRebase)
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if got := parents(t, f.dir, head); len(got) != 1 {
		t.Fatalf("ordinary rebase produced a merge bridge with %d parents: %v", len(got), got)
	}
	if gitStatusPorcelain(t, f.dir) != "" {
		t.Fatal("ordinary rebase left a dirty worktree")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "feature.txt")); err != nil {
		t.Fatalf("ordinary rebase lost feature content: %v", err)
	}
}

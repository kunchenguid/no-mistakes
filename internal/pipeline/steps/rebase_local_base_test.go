package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #283: a gated branch built on top of a local pipeline base that is
// ahead of origin carries that other workstream's committed-but-unpushed
// commits. Rebasing onto the fresh remote pipeline base keeps them in the
// branch's history, so the PR silently bundles unrelated work.
//
// The rebase step must detect that the branch carries commits which exist on
// the local pipeline base but were never pushed to origin/<base>, and stop
// for review instead of silently widening the PR.
func TestRebaseStep_DetectsUnpushedLocalPipelineBaseCommits(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	// Working repo: local pipeline base (main) advances with an unrelated
	// workstream's commit that is never pushed to origin.
	working := t.TempDir()
	gitCmd(t, working, "init")
	gitCmd(t, working, "config", "user.name", "test")
	gitCmd(t, working, "config", "user.email", "test@test.com")
	gitCmd(t, working, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(working, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "base")
	d0 := gitCmd(t, working, "rev-parse", "HEAD")
	gitCmd(t, working, "remote", "add", "origin", upstream)
	gitCmd(t, working, "push", "origin", "main")
	gitCmd(t, working, "checkout", "-b", "staging")
	gitCmd(t, working, "push", "origin", "staging") // origin/staging == D0

	// Unrelated workstream commits to local pipeline base but does NOT push.
	os.WriteFile(filepath.Join(working, "unrelated_a.txt"), []byte("backend a"), 0o644)
	os.WriteFile(filepath.Join(working, "unrelated_b.txt"), []byte("backend b"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "unrelated backend work (77 files)")
	localBaseTip := gitCmd(t, working, "rev-parse", "HEAD") // D0 + U, unpushed

	// Gate worktree: feature was branched off the local (ahead) staging, so it
	// carries the unrelated commit U as an ancestor.
	dir := t.TempDir()
	gitCmd(t, dir, "clone", upstream, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "fetch", working, "staging") // import U's objects (as feature ancestor)
	gitCmd(t, dir, "checkout", "--detach", localBaseTip)
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "my_fix.txt"), []byte("my 2-line fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "my fix")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD") // D0 + U + M

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, d0, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.BaseBranch = "staging"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.WorkingPath = working
	sctx.Repo.BaseBranch = "release/v2"

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the rebase step to stop for review when the branch bundles unpushed local-base commits, got outcome=%#v", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("bundling unpushed local-base commits is not safely auto-fixable")
	}
	if !strings.Contains(outcome.Findings, "unrelated backend work") &&
		!strings.Contains(strings.ToLower(outcome.Findings), "local") {
		t.Fatalf("expected findings to flag the unpushed local-pipeline-base commits, got: %s", outcome.Findings)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected one finding, got %d", len(findings.Items))
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want %q", findings.Items[0].Action, types.ActionAskUser)
	}
}

func TestRebaseStep_DetectsUnpushedLocalPipelineBaseCommitsOnForcePush(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	working := t.TempDir()
	gitCmd(t, working, "init")
	gitCmd(t, working, "config", "user.name", "test")
	gitCmd(t, working, "config", "user.email", "test@test.com")
	gitCmd(t, working, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(working, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "base")
	gitCmd(t, working, "remote", "add", "origin", upstream)
	gitCmd(t, working, "push", "origin", "main")
	gitCmd(t, working, "checkout", "-b", "staging")
	gitCmd(t, working, "push", "origin", "staging")

	os.WriteFile(filepath.Join(working, "unrelated_force.txt"), []byte("local pipeline base work"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "unrelated local pipeline base work")
	localBaseTip := gitCmd(t, working, "rev-parse", "HEAD")

	dir := t.TempDir()
	gitCmd(t, dir, "clone", upstream, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "old_feature.txt"), []byte("old feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "old feature")
	oldFeatureSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "fetch", working, "staging")
	gitCmd(t, dir, "checkout", "--detach", localBaseTip)
	gitCmd(t, dir, "checkout", "-B", "feature")
	os.WriteFile(filepath.Join(dir, "my_force_fix.txt"), []byte("force-pushed fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "force-pushed fix")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, oldFeatureSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.BaseBranch = "staging"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.WorkingPath = working
	sctx.Repo.BaseBranch = "release/v2"

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected force-push rebase to stop for bundled local-base commits, got outcome=%#v", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("bundled local-base commits on a force push are not safely auto-fixable")
	}
	if !strings.Contains(outcome.Findings, "unrelated local pipeline base work") {
		t.Fatalf("expected findings to mention the bundled local pipeline base commit, got: %s", outcome.Findings)
	}
}

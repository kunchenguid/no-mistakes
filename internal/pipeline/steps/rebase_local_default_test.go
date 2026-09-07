package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #283: a gated branch built on top of a local default branch that is
// ahead of origin carries that other workstream's committed-but-unpushed
// commits. Rebasing onto the fresh remote default branch keeps them in the
// branch's history, so the PR silently bundles unrelated work.
//
// The rebase step must detect that the branch carries commits which exist on
// the local default branch but were never pushed to origin/<default>, and stop
// for review instead of silently widening the PR.
func TestRebaseStep_DetectsUnpushedLocalDefaultBranchCommits(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	// Working repo: local default branch (main) advances with an unrelated
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
	gitCmd(t, working, "push", "origin", "main") // origin/main == D0

	// Unrelated workstream commits to local main but does NOT push.
	os.WriteFile(filepath.Join(working, "unrelated_a.txt"), []byte("backend a"), 0o644)
	os.WriteFile(filepath.Join(working, "unrelated_b.txt"), []byte("backend b"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "unrelated backend work (77 files)")
	localMainTip := gitCmd(t, working, "rev-parse", "HEAD") // D0 + U, unpushed

	// Gate worktree: feature was branched off the local (ahead) main, so it
	// carries the unrelated commit U as an ancestor.
	dir := t.TempDir()
	gitCmd(t, dir, "clone", upstream, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "fetch", working, "main") // import U's objects (as feature ancestor)
	gitCmd(t, dir, "checkout", "--detach", localMainTip)
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "my_fix.txt"), []byte("my 2-line fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "my fix")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD") // D0 + U + M

	// Upstream-only files must not inflate the proposed PR evidence.
	gitCmd(t, working, "checkout", "-b", "upstream-advance", d0)
	if err := os.WriteFile(filepath.Join(working, "aaa_upstream_only.txt"), []byte("upstream"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "advance upstream independently")
	gitCmd(t, working, "push", "origin", "HEAD:main")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, d0, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.WorkingPath = working

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the rebase step to stop for review when the branch bundles unpushed local-default commits, got outcome=%#v", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("bundling unpushed local-default commits is not safely auto-fixable")
	}
	if !strings.Contains(outcome.Findings, "unrelated backend work") &&
		!strings.Contains(strings.ToLower(outcome.Findings), "local") {
		t.Fatalf("expected findings to flag the unpushed local-default-branch commits, got: %s", outcome.Findings)
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
	if !strings.Contains(outcome.Findings, "proposed PR changes 3 file(s)") || findings.Items[0].File != "my_fix.txt" {
		t.Fatalf("expected actual PR file evidence: %s", outcome.Findings)
	}
	sctx.Fixing = true
	outcome, err = step.Execute(sctx)
	if err != nil || !outcome.NeedsApproval || !strings.Contains(outcome.FixSummary, "no changes applied") {
		t.Fatalf("expected explicit unsupported fix: %#v, %v", outcome, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("unsupported fix moved HEAD to %s", got)
	}
}

func TestRebaseStep_DetectsUnpushedLocalDefaultBranchCommitsOnForcePush(t *testing.T) {
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

	os.WriteFile(filepath.Join(working, "unrelated_force.txt"), []byte("local main work"), 0o644)
	gitCmd(t, working, "add", "-A")
	gitCmd(t, working, "commit", "-m", "unrelated local main work")
	localMainTip := gitCmd(t, working, "rev-parse", "HEAD")

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

	gitCmd(t, dir, "fetch", working, "main")
	gitCmd(t, dir, "checkout", "--detach", localMainTip)
	gitCmd(t, dir, "checkout", "-B", "feature")
	os.WriteFile(filepath.Join(dir, "my_force_fix.txt"), []byte("force-pushed fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "force-pushed fix")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, oldFeatureSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.WorkingPath = working

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected force-push rebase to stop for bundled local default commits, got outcome=%#v", outcome)
	}
	if outcome.AutoFixable {
		t.Fatalf("bundled local default commits on a force push are not safely auto-fixable")
	}
	if !strings.Contains(outcome.Findings, "unrelated local main work") {
		t.Fatalf("expected findings to mention the bundled local main commit, got: %s", outcome.Findings)
	}
}

// Issue #998: committing intended work on main before naming its delivery
// branch is not evidence of a separate bundled workstream.
func TestRebaseStep_LocalDefaultTipIsIntendedDelivery(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	for _, name := range []string{"package.json", "package-lock.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("base"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	base := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	for _, name := range []string{"package.json", "package-lock.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("upgrade"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "intended dependency upgrade")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-b", "feature")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Repo.WorkingPath = dir
	for _, fixing := range []bool{false, true} {
		sctx.Fixing = fixing
		outcome, err := (&RebaseStep{}).Execute(sctx)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.NeedsApproval || outcome.SkipRemaining {
			t.Fatalf("intended work rejected: %#v", outcome)
		}
		if fixing && !strings.Contains(outcome.FixSummary, "no changes applied") {
			t.Fatalf("no-op misreported: %#v", outcome)
		}
		if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
			t.Fatalf("HEAD moved: %s", got)
		}
	}
}

package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const preserveBase = "captain/preserve-firstmate-project-touch-d78"

func baseBranchContext(t *testing.T, explicit string) *pipeline.StepContext {
	t.Helper()
	workDir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, workDir, baseSHA, headSHA, config.Commands{})
	sctx.Config.BaseBranch = explicit
	return sctx
}

// Absent explicit configuration must resolve exactly to today's behavior.
func TestBaseBranch_FallsBackToRepoDefaultBranch(t *testing.T) {
	sctx := baseBranchContext(t, "")
	if got := baseBranch(sctx); got != "main" {
		t.Fatalf("baseBranch = %q, want the repo default branch %q", got, "main")
	}
}

func TestBaseBranch_ExplicitOverridesRepoDefaultBranch(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	if got := baseBranch(sctx); got != preserveBase {
		t.Fatalf("baseBranch = %q, want the explicit base %q", got, preserveBase)
	}
	// The repo record is untouched: DefaultBranch keeps its separate jobs as
	// the daemon's trusted-config anchor and the telemetry branch role.
	if sctx.Repo.DefaultBranch != "main" {
		t.Fatalf("Repo.DefaultBranch = %q, want it left at %q", sctx.Repo.DefaultBranch, "main")
	}
}

func TestAssertBaseBranchUsable_NoExplicitBaseIsAlwaysUsable(t *testing.T) {
	sctx := baseBranchContext(t, "")
	if err := assertBaseBranchUsable(sctx); err != nil {
		t.Fatalf("assertBaseBranchUsable = %v, want nil without an explicit base", err)
	}
}

func TestAssertBaseBranchUsable_AcceptsValidExplicitBase(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	if err := assertBaseBranchUsable(sctx); err != nil {
		t.Fatalf("assertBaseBranchUsable = %v, want nil", err)
	}
}

func TestAssertBaseBranchUsable_RejectsUnsafeRef(t *testing.T) {
	for _, unsafe := range []string{"--upload-pack=touch /tmp/pwn", "refs/heads/main", "main..feature", "captain/*"} {
		t.Run(unsafe, func(t *testing.T) {
			sctx := baseBranchContext(t, unsafe)
			err := assertBaseBranchUsable(sctx)
			if err == nil {
				t.Fatalf("assertBaseBranchUsable(%q) = nil, want a refusal", unsafe)
			}
			if !strings.Contains(err.Error(), "explicit base branch rejected") {
				t.Fatalf("error %q does not identify the explicit base as the cause", err)
			}
		})
	}
}

// A branch that is its own base has an empty reviewed delta and would open a
// self-targeting PR, so it must stop the run rather than gate nothing.
func TestAssertBaseBranchUsable_RejectsSelfAsBase(t *testing.T) {
	sctx := baseBranchContext(t, "feature")
	sctx.Run.Branch = "refs/heads/feature"
	err := assertBaseBranchUsable(sctx)
	if err == nil {
		t.Fatal("assertBaseBranchUsable = nil, want a refusal when the branch is its own base")
	}
	if !strings.Contains(err.Error(), "cannot be its own base") {
		t.Fatalf("error %q does not explain the self-base refusal", err)
	}
}

func TestAssertBaseBranchResolvable_NoExplicitBaseSkipsRemoteCheck(t *testing.T) {
	sctx := baseBranchContext(t, "")
	sctx.Repo.UpstreamURL = "nonexistent-remote"
	if err := assertBaseBranchResolvable(context.Background(), sctx); err != nil {
		t.Fatalf("assertBaseBranchResolvable = %v, want nil without an explicit base", err)
	}
}

func TestAssertBaseBranchResolvable_AcceptsExistingRemoteBranch(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	remote := newBareRemoteWithBranches(t, sctx.WorkDir, preserveBase)
	sctx.Repo.UpstreamURL = gitCmd(t, sctx.WorkDir, "remote", "get-url", remote)
	if err := assertBaseBranchResolvable(context.Background(), sctx); err != nil {
		t.Fatalf("assertBaseBranchResolvable = %v, want nil for an existing base", err)
	}
}

func TestAssertBaseBranchResolvable_RefusesMissingRemoteBranch(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	remote := newBareRemoteWithBranches(t, sctx.WorkDir)
	sctx.Repo.UpstreamURL = gitCmd(t, sctx.WorkDir, "remote", "get-url", remote)
	err := assertBaseBranchResolvable(context.Background(), sctx)
	if err == nil {
		t.Fatal("assertBaseBranchResolvable = nil, want a refusal for a base absent from the remote")
	}
	if !strings.Contains(err.Error(), "does not exist on the remote") {
		t.Fatalf("error %q does not explain the unresolved base", err)
	}
}

func TestAssertBaseBranchResolvable_RefusesUnreachableRemote(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	sctx.Repo.UpstreamURL = "no-such-remote"
	err := assertBaseBranchResolvable(context.Background(), sctx)
	if err == nil {
		t.Fatal("assertBaseBranchResolvable = nil, want a refusal when the remote cannot be queried")
	}
	if !strings.Contains(err.Error(), "could not resolve explicit base branch") {
		t.Fatalf("error %q does not fail closed on an unreachable remote", err)
	}
}

func TestAssertBaseBranchResolvable_UsesVerifiedUpstreamInsteadOfStaleOrigin(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	staleRemote := newBareRemoteWithBranches(t, sctx.WorkDir)
	gitCmd(t, sctx.WorkDir, "remote", "rename", staleRemote, "origin")
	refreshedRemote := newBareRemoteWithBranches(t, sctx.WorkDir, preserveBase)
	sctx.Repo.UpstreamURL = gitCmd(t, sctx.WorkDir, "remote", "get-url", refreshedRemote)
	sctx.Repo.URLsVerified = true

	if err := assertBaseBranchResolvable(context.Background(), sctx); err != nil {
		t.Fatalf("assertBaseBranchResolvable = %v, want verified upstream base to resolve", err)
	}
}

func TestPrepareExplicitBaseBranchFetchesRemoteTrackingRef(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	remote := newBareRemoteWithBranches(t, sctx.WorkDir, preserveBase)
	sctx.Repo.UpstreamURL = gitCmd(t, sctx.WorkDir, "remote", "get-url", remote)

	if err := PrepareExplicitBaseBranch(context.Background(), sctx); err != nil {
		t.Fatal(err)
	}
	got := gitCmd(t, sctx.WorkDir, "rev-parse", "refs/remotes/origin/"+preserveBase)
	want := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	if got != want {
		t.Fatalf("fetched explicit base = %s, want %s", got, want)
	}
}

// The rebase step is the first step that acts on the base; a bad explicit base
// must stop it before any fetch, rebase, or later push/PR mutation.
func TestRebaseStep_RefusesUnsafeExplicitBaseBeforeMutating(t *testing.T) {
	sctx := baseBranchContext(t, "--upload-pack=touch /tmp/pwn")
	headBefore := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")

	step := &RebaseStep{}
	outcome, err := step.Execute(sctx)
	if err == nil {
		t.Fatalf("Execute = %+v, nil error; want a refusal", outcome)
	}
	if !strings.Contains(err.Error(), "explicit base branch rejected") {
		t.Fatalf("error %q does not identify the explicit base as the cause", err)
	}
	if head := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"); head != headBefore {
		t.Fatalf("HEAD moved to %s from %s; the refusal must precede any mutation", head, headBefore)
	}
}

func TestRebaseStep_RefusesUnresolvedExplicitBaseBeforeMutating(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	// The step resolves the remote from the repo's upstream URL, defaulting to
	// "origin"; wire a reachable origin that simply lacks the configured base.
	remoteDir := t.TempDir()
	gitCmd(t, remoteDir, "init", "--bare", "--initial-branch=main", ".")
	gitCmd(t, sctx.WorkDir, "remote", "add", "origin", remoteDir)
	gitCmd(t, sctx.WorkDir, "push", "origin", "HEAD:refs/heads/main")
	headBefore := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")

	step := &RebaseStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("Execute = nil error; want a refusal for a base absent from the remote")
	} else if !strings.Contains(err.Error(), "does not exist on the remote") {
		t.Fatalf("error %q does not explain the unresolved base", err)
	}
	if head := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"); head != headBefore {
		t.Fatalf("HEAD moved to %s from %s; the refusal must precede any mutation", head, headBefore)
	}
}

func TestRebaseStep_RebasesOntoExplicitBaseInsteadOfDefaultBranch(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare", "--initial-branch=main", ".")

	workDir := t.TempDir()
	gitCmd(t, workDir, "init", "--initial-branch=main")
	gitCmd(t, workDir, "config", "user.name", "test")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "remote", "add", "origin", upstream)
	if err := os.WriteFile(filepath.Join(workDir, "base.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "-A")
	gitCmd(t, workDir, "commit", "-m", "main base")
	mainSHA := gitCmd(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "origin", "main")

	gitCmd(t, workDir, "checkout", "-b", preserveBase)
	if err := os.WriteFile(filepath.Join(workDir, "explicit-base.txt"), []byte("required base change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "-A")
	gitCmd(t, workDir, "commit", "-m", "explicit base change")
	explicitBaseSHA := gitCmd(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "origin", preserveBase)

	gitCmd(t, workDir, "checkout", "-b", "feature", mainSHA)
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("feature change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "-A")
	gitCmd(t, workDir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "origin", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, workDir, mainSHA, headSHA, config.Commands{})
	sctx.Config.BaseBranch = preserveBase
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("clean explicit-base rebase unexpectedly needs approval: %s", outcome.Findings)
	}
	if got := gitCmd(t, workDir, "merge-base", "HEAD", "origin/"+preserveBase); got != explicitBaseSHA {
		t.Fatalf("HEAD merge-base with explicit base = %s, want %s", got, explicitBaseSHA)
	}
	if got := gitCmd(t, workDir, "merge-base", "HEAD", "origin/main"); got == gitCmd(t, workDir, "rev-parse", "HEAD") {
		t.Fatal("feature collapsed onto the default branch instead of retaining its own change")
	}
	if _, err := os.Stat(filepath.Join(workDir, "explicit-base.txt")); err != nil {
		t.Fatalf("rebased feature does not contain the explicit base change: %v", err)
	}
	t.Logf("feature HEAD %s includes explicit base %s and its explicit-base.txt change; default main remains %s",
		gitCmd(t, workDir, "rev-parse", "HEAD"), explicitBaseSHA, mainSHA)
}

// The PR step must target the explicit base, and must keep targeting the repo
// default branch when none is configured.
func TestPRStep_BaseSelectionFollowsExplicitBase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit string
		wantBase string
	}{
		{"default base preserved", "", "main"},
		{"explicit base honored", preserveBase, preserveBase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx := baseBranchContext(t, tc.explicit)
			if got := baseBranch(sctx); got != tc.wantBase {
				t.Fatalf("PR base = %q, want %q", got, tc.wantBase)
			}
		})
	}
}

// A run whose branch equals the base must skip PR creation instead of opening a
// PR from a branch onto itself.
func TestPRStep_SkipsWhenBranchIsTheExplicitBase(t *testing.T) {
	sctx := baseBranchContext(t, preserveBase)
	sctx.Run.Branch = "refs/heads/" + preserveBase
	sctx.Repo = &db.Repo{ID: "repo-1", WorkingPath: sctx.WorkDir, UpstreamURL: "https://github.com/test/repo", DefaultBranch: "main"}

	// assertBaseBranchUsable is what the step consults first; it must refuse
	// before the step reaches any SCM host call.
	if err := assertBaseBranchUsable(sctx); err == nil {
		t.Fatal("assertBaseBranchUsable = nil, want a refusal when the branch is its own base")
	}
}

// newBareRemoteWithBranches creates a bare repo carrying the named branches,
// registers it as a remote of workDir, and returns the remote name.
func newBareRemoteWithBranches(t *testing.T, workDir string, branches ...string) string {
	t.Helper()
	remoteDir := t.TempDir()
	gitCmd(t, remoteDir, "init", "--bare", "--initial-branch=main", ".")

	const remote = "basecheck"
	gitCmd(t, workDir, "remote", "add", remote, remoteDir)
	gitCmd(t, workDir, "push", remote, "HEAD:refs/heads/main")
	for _, branch := range branches {
		gitCmd(t, workDir, "push", remote, "HEAD:refs/heads/"+branch)
	}
	return remote
}

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newCLIRebaseOnlyDeadlockFixture reproduces the exact operator-visible
// deadlock reported on personal-build, where all three causes stack on one
// branch:
//
//  1. the owning run only rebased, so it committed from a detached pipeline
//     worktree: the preserved head exists in the gate's object store while the
//     gate branch ref is still exactly the submitted head;
//  2. that run died mid-step, so it carries a terminal status with no
//     terminal_head_verified_at stamp; and
//  3. an older terminal run on the same branch also holds unpublished custody
//     with its head still at the submitted head, so it re-blocks the branch the
//     moment the newer run's custody is returned.
//
// The union is what made `axi sync --check` advertise recover_custody and
// `axi sync --recover` then refuse, with no supported way back to a fresh run.
func newCLIRebaseOnlyDeadlockFixture(t *testing.T) cliRecoverFixture {
	t.Helper()
	nmHome := filepath.Join(t.TempDir(), "nm-home")
	t.Setenv("NM_HOME", nmHome)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	cliGit(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	base := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "-b", "personal-build")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "operator work")
	submitted := cliGit(t, local, "rev-parse", "HEAD")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(registeredRoot, remote, "main")
	if err != nil {
		t.Fatal(err)
	}

	gate := p.RepoDir(repo.ID)
	cliGit(t, filepath.Dir(gate), "init", "--bare", gate)
	cliGit(t, local, "push", gate, "refs/heads/personal-build:refs/heads/personal-build")

	// Cause 1: the pipeline rebased the operator's commit onto an advanced
	// base from a detached worktree. The rebased head reaches the gate's
	// object store, the gate branch ref never moves off the submitted head.
	pipeline := filepath.Join(root, "pipeline")
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "--detach", "origin/personal-build")
	cliGit(t, pipeline, "commit", "--amend", "-m", "operator work (rebased by the pipeline)")
	preserved := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", "origin", "HEAD:refs/heads/tmp-detached-delivery")
	cliGit(t, gate, "update-ref", "-d", "refs/heads/tmp-detached-delivery")
	if got := cliGit(t, gate, "rev-parse", "refs/heads/personal-build"); got != submitted {
		t.Fatalf("gate branch ref = %s, want the submitted head %s", got, submitted)
	}

	// Cause 3: an older terminal run that never moved the head still holds
	// unpublished custody (it died before writing a head verification too).
	older, err := database.InsertRun(repo.ID, "personal-build", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(older.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	// The owning rebase-only run: head advanced to the detached preserved
	// commit, terminal, and (cause 2) never head-verified.
	run, err := database.InsertRun(repo.ID, "personal-build", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliRecoverFixture{local: local, gate: gate, submitted: submitted, preserved: preserved, runID: run.ID}
}

// TestAxiSyncRecoverBreaksTheRebaseOnlyCustodyDeadlock walks the operator's own
// CLI surfaces through the reported deadlock: the guarded check advertises
// recover_custody, the recovery must then actually return custody instead of
// refusing, and the branch must stay recovered afterwards rather than being
// re-blocked by the older terminal run.
func TestAxiSyncRecoverBreaksTheRebaseOnlyCustodyDeadlock(t *testing.T) {
	f := newCLIRebaseOnlyDeadlockFixture(t)

	check, err := executeCmd("axi", "sync", "--check")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("stranded check should exit 1, got %#v\n%s", err, check)
	}
	t.Logf("axi sync --check (before recovery):\n%s", check)
	for _, want := range []string{
		"state: pipeline_owned",
		"safety: blocked_pipeline_owned_recoverable",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover",
	} {
		if !strings.Contains(check, want) {
			t.Errorf("advertised recovery missing %q:\n%s", want, check)
		}
	}

	recover, err := executeCmd("axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("advertised recovery refused instead of returning custody: %v\n%s", err, recover)
	}
	t.Logf("axi sync --recover:\n%s", recover)
	for _, want := range []string{"recovered: true", "state: custody_returned", "changed: true", "no-mistakes axi run --intent"} {
		if !strings.Contains(recover, want) {
			t.Errorf("recovery output missing %q:\n%s", want, recover)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("worktree HEAD = %s, want the preserved pipeline head %s", got, f.preserved)
	}
	if out := cliGit(t, f.local, "status", "--porcelain"); out != "" {
		t.Fatalf("worktree not clean after recovery: %q", out)
	}
	anchor := "refs/no-mistakes/recover/" + f.runID
	if got := cliGit(t, f.local, "rev-parse", anchor); got != f.preserved {
		t.Fatalf("preserved anchor %s = %s, want %s", anchor, got, f.preserved)
	}

	// The older terminal run must not reclaim the branch it can never prove
	// custody for again: the recovered branch stays usable.
	after, err := executeCmd("axi", "sync", "--check")
	if err != nil {
		t.Fatalf("post-recovery check should exit 0: %v\n%s", err, after)
	}
	t.Logf("axi sync --check (after recovery):\n%s", after)
	if !strings.Contains(after, "state: custody_returned") {
		t.Fatalf("older terminal run re-blocked the recovered branch:\n%s", after)
	}
	if strings.Contains(after, "blocked_") {
		t.Fatalf("post-recovery check still reports a block:\n%s", after)
	}
}

// TestAxiSyncRecoverStillRefusesADetachedHeadTheGateNeverReceived pins the
// fail-closed half of the same path at the CLI surface: when the recorded head
// is not in the gate at all, the unverified-head proof must not be satisfied by
// the branch ref sitting at the submitted head.
func TestAxiSyncRecoverStillRefusesADetachedHeadTheGateNeverReceived(t *testing.T) {
	f := newCLIRebaseOnlyDeadlockFixture(t)
	// Drop the preserved commit from the gate's object store; the branch ref
	// stays exactly where the rebase-only run left it.
	cliGit(t, f.gate, "gc", "--prune=now")
	if _, err := git.Run(t.Context(), f.gate, "cat-file", "-e", f.preserved+"^{commit}"); err == nil {
		t.Skip("gate still holds the preserved commit after pruning; cannot model the never-received head")
	}

	out, err := executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("recovery of a head the gate never received should exit 1, got %#v\n%s", err, out)
	}
	t.Logf("axi sync --recover (head the gate never received):\n%s", out)
	if !strings.Contains(out, "blocked_recover_unverified_head") {
		t.Errorf("refusal missing the unverified-head safety code:\n%s", out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("refused recovery moved HEAD to %s", got)
	}
}

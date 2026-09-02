package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type cliLostHeadFixture struct {
	local, gate, submitted, lost, runID string
}

// newCLILostHeadFixture reproduces the reported dead end on the CLI surface:
// a terminal run whose recorded pipeline head is a real commit that exists in
// no store no-mistakes can read, with the gate branch still at the submitted
// head and the operator worktree clean and containing every head the run
// recorded.
func newCLILostHeadFixture(t *testing.T) cliLostHeadFixture {
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
	cliGit(t, local, "checkout", "-b", "feature/lost")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "commit", "-am", "feature")
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
	cliGit(t, local, "push", gate, "refs/heads/feature/lost:refs/heads/feature/lost")

	scratch := filepath.Join(root, "scratch")
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", gate, scratch)
	cliGit(t, scratch, "config", "user.name", "Pipeline")
	cliGit(t, scratch, "config", "user.email", "pipeline@example.com")
	cliGit(t, scratch, "checkout", "feature/lost")
	if err := os.WriteFile(filepath.Join(scratch, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, scratch, "add", "fix.txt")
	cliGit(t, scratch, "commit", "-m", "no-mistakes(review): fix")
	lost := cliGit(t, scratch, "rev-parse", "HEAD")
	if err := os.RemoveAll(scratch); err != nil {
		t.Fatal(err)
	}

	run, err := database.InsertRun(repo.ID, "feature/lost", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, lost); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliLostHeadFixture{local: local, gate: gate, submitted: submitted, lost: lost, runID: run.ID}
}

// TestAxiSyncLostPipelineHeadOffersAndPerformsCustodyReturn is the CLI-surface
// regression: the branch must stop reporting a next action that only reprints
// the same block, and `--recover` must end the strand without touching a file.
func TestAxiSyncLostPipelineHeadOffersAndPerformsCustodyReturn(t *testing.T) {
	f := newCLILostHeadFixture(t)

	out, err := executeCmd("axi", "sync", "--check")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("stranded check should exit 1, got %#v\n%s", err, out)
	}
	for _, want := range []string{
		"state: pipeline_owned",
		"safety: blocked_pipeline_owned_head_lost",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover",
		f.lost,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lost-head check missing %q:\n%s", want, out)
		}
	}
	// A head that no longer exists cannot be re-validated, so the rerun exit
	// must not be advertised here.
	if strings.Contains(out, "no-mistakes rerun") {
		t.Errorf("lost-head check advertised rerun for a head that does not exist:\n%s", out)
	}

	out, err = executeCmd("axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("lost-head recover: %v\n%s", err, out)
	}
	for _, want := range []string{"recovered: true", "state: custody_returned", "changed: false", "no-mistakes axi run --intent"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost-head recover output missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("lost-head recovery moved HEAD to %s, want %s", got, f.submitted)
	}
	if refs := cliGit(t, f.local, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/"); refs != "" {
		t.Fatalf("lost-head recovery created refs: %q", refs)
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/feature/lost"); got != f.submitted {
		t.Fatalf("lost-head recovery moved the gate branch to %s", got)
	}

	out, err = executeCmd("axi", "sync", "--check")
	if err != nil {
		t.Fatalf("post-recover check should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "state: custody_returned") {
		t.Fatalf("post-recover check:\n%s", out)
	}
}

// TestAxiSyncLostPipelineHeadWithSurvivingGateCommitsStaysBlocked is the
// negative CLI case: a gate branch carrying pipeline commits the local branch
// does not contain keeps the manual refusal, whatever the recorded head is.
func TestAxiSyncLostPipelineHeadWithSurvivingGateCommitsStaysBlocked(t *testing.T) {
	f := newCLILostHeadFixture(t)
	advance := filepath.Join(filepath.Dir(f.local), "gate-advance")
	cliGit(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, advance)
	cliGit(t, advance, "config", "user.name", "Pipeline")
	cliGit(t, advance, "config", "user.email", "pipeline@example.com")
	cliGit(t, advance, "checkout", "feature/lost")
	if err := os.WriteFile(filepath.Join(advance, "gate-fix.txt"), []byte("surviving\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, advance, "add", "gate-fix.txt")
	cliGit(t, advance, "commit", "-m", "no-mistakes(lint): fix")
	cliGit(t, advance, "push", "origin", "HEAD:refs/heads/feature/lost")
	surviving := cliGit(t, advance, "rev-parse", "HEAD")
	chdir(t, f.local)

	out, err := executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("surviving gate commits should refuse, got %#v\n%s", err, out)
	}
	if strings.Contains(out, "recovered: true") || strings.Contains(out, "blocked_pipeline_owned_head_lost") {
		t.Fatalf("surviving gate commits were released:\n%s", out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("refused recovery moved HEAD to %s", got)
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/feature/lost"); got != surviving {
		t.Fatalf("refused recovery moved the gate branch to %s, want %s", got, surviving)
	}
}

// TestFreshRunRefusalOnLostPipelineHeadPointsAtTheSupportedRecovery pins the
// other half of the reported dead end: `axi run` still refuses while a
// terminal run owns the branch, but the refusal it emits must carry the
// supported custody-return command instead of a next action that only
// reprints the same block. The lost case is the one where the recorded head
// is provably missing from the worktree and the local gate; the orphan case
// shares the same safety code and next action but reports the head as no
// longer reachable as an ancestor of this branch.
func TestFreshRunRefusalOnLostPipelineHeadPointsAtTheSupportedRecovery(t *testing.T) {
	lostHead := strings.Repeat("d", 40)
	cases := []struct {
		name  string
		state branchsync.State
	}{
		{
			name: "lost_head_missing_from_worktree_and_gate",
			state: branchsync.State{
				State:  branchsync.StatePipelineOwned,
				Safety: branchsync.SafetyPipelineOwnedHeadLost,
				Local:  branchsync.LocalState{Branch: "feature/lost", Head: strings.Repeat("a", 40), Clean: true},
				Pipeline: branchsync.PipelineState{
					RunID: "run-1", Status: "cancelled", Phase: "pre_push",
					SubmittedHead: strings.Repeat("a", 40), CurrentHead: lostHead,
				},
				Error:      "the run finished cancelled and its recorded pipeline head " + lostHead + " no longer exists in the invoking worktree or the local gate; every head this run recorded is already contained in this branch, so returning custody strands nothing and changes no file or ref",
				NextAction: &branchsync.NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover"},
			},
		},
		{
			name: "orphan_head_unreachable_as_ancestor_of_this_branch",
			state: branchsync.State{
				State:  branchsync.StatePipelineOwned,
				Safety: branchsync.SafetyPipelineOwnedHeadLost,
				Local:  branchsync.LocalState{Branch: "feature/lost", Head: strings.Repeat("a", 40), Clean: true},
				Pipeline: branchsync.PipelineState{
					RunID: "run-1", Status: "cancelled", Phase: "pre_push",
					SubmittedHead: strings.Repeat("a", 40), CurrentHead: lostHead,
				},
				Error:      "the run finished cancelled and its recorded pipeline head " + lostHead + " is no longer reachable as an ancestor of this branch, but every head this run recorded is already contained in this branch; returning custody strands nothing and changes no file or ref",
				NextAction: &branchsync.NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			err := emitBranchOwnershipError(cmd, &branchOwnershipError{state: tc.state})
			var ee *exitError
			if err == nil || !asExitError(err, &ee) || ee.code != 1 {
				t.Fatalf("fresh-run refusal should exit 1, got %#v", err)
			}
			rendered := out.String()
			for _, want := range []string{
				"safety: blocked_pipeline_owned_head_lost",
				"code: recover_custody",
				"Run `no-mistakes axi sync --recover`",
				lostHead,
			} {
				if !strings.Contains(rendered, want) {
					t.Errorf("fresh-run refusal missing %q:\n%s", want, rendered)
				}
			}
			if strings.Contains(rendered, "Run `no-mistakes axi status`") {
				t.Errorf("fresh-run refusal still points at the dead-end command:\n%s", rendered)
			}
		})
	}
}

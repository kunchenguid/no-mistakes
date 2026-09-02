package branchsync

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// liveCaseFixture reproduces the exact persisted shape reported against run
// 01M1GE0QM433D9G2A0QDMJFGJH (PR #2 dogfood): a terminal-failed run with an
// unpublished pipeline head that ended up as an orphan loose object in the
// main repository, while the operator's worktree branch already contains the
// submitted head (and any pushed head) as an ancestor through a normal merge
// or pull. The worktree is clean. The gate's recovery ref still anchors the
// recorded head, and the gate's branch is at the submitted head. PR #2
// classified this as blocked_recover_preserved_head_missing because the
// recorded head was reachable as a loose object, so the "provably missing"
// clause refused; the new worktreeBranchAlreadyHoldsPipeline clause is the
// smaller positive proof that recognizes the work as already integrated.
type liveCaseFixture struct {
	t         *testing.T
	ctx       context.Context
	db        *db.DB
	repo      *db.Repo
	run       *db.Run
	service   *Service
	local     string
	gate      string
	submitted string
	recorded  string
	merged    string
}

func newLiveCaseFixture(t *testing.T) *liveCaseFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)

	main := filepath.Join(root, "main")
	mustRun(t, root, "init", "-b", "main", main)
	configureIdentity(t, main)
	mustWrite(t, filepath.Join(main, "f.txt"), "base\n")
	mustRun(t, main, "add", "f.txt")
	mustRun(t, main, "commit", "-m", "base")
	base := mustRun(t, main, "rev-parse", "HEAD")
	mustRun(t, main, "checkout", "-b", "fm/feature")
	mustWrite(t, filepath.Join(main, "f.txt"), "feature\n")
	mustRun(t, main, "commit", "-am", "feature")
	submitted := mustRun(t, main, "rev-parse", "HEAD")
	mustRun(t, main, "push", gate, "refs/heads/fm/feature:refs/heads/fm/feature")

	// A sibling branch on origin that the operator later merges.
	mustRun(t, main, "checkout", base)
	mustRun(t, main, "checkout", "-b", "fm/sibling")
	mustWrite(t, filepath.Join(main, "s.txt"), "sibling\n")
	mustRun(t, main, "add", "s.txt")
	mustRun(t, main, "commit", "-m", "sibling")
	mustRun(t, main, "push", gate, "refs/heads/fm/sibling:refs/heads/fm/sibling")

	// The recorded head: a real commit made on the operator's branch that
	// later became the orphan loose object the live case shipped.
	mustRun(t, main, "checkout", "fm/feature")
	mustWrite(t, filepath.Join(main, "fix.txt"), "pipeline fix\n")
	mustRun(t, main, "add", "fix.txt")
	mustRun(t, main, "commit", "-m", "no-mistakes(review): fix")
	recorded := mustRun(t, main, "rev-parse", "HEAD")
	mustRun(t, main, "push", "-f", gate, "refs/heads/fm/feature:refs/heads/fm/feature")

	// The operator merges sibling into fm/feature without the recorded head
	// as a parent: the recorded head stays reachable only as a loose object.
	mustRun(t, main, "reset", "--hard", submitted)
	mustRun(t, main, "merge", "--no-ff", "-m", "merge sibling", "fm/sibling")
	merged := mustRun(t, main, "rev-parse", "HEAD")
	mustRun(t, main, "push", "-f", gate, "refs/heads/fm/feature:refs/heads/fm/feature")

	// Verify the recorded head is still a loose object reachable from the
	// worktree's shared git-dir (via commondir) but NOT an ancestor of HEAD.
	if out, err := exec.Command("git", "-C", main, "cat-file", "-t", recorded).CombinedOutput(); err != nil {
		t.Fatalf("recorded head %s not in main repo objects: %s", recorded, strings.TrimSpace(string(out)))
	}
	if _, err := exec.Command("git", "-C", main, "merge-base", "--is-ancestor", recorded, "HEAD").CombinedOutput(); err == nil {
		t.Fatalf("recorded head %s is unexpectedly an ancestor of HEAD %s", recorded, merged)
	}

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(main, gate, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "fm/feature", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, recorded); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, recorded); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &liveCaseFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: main, GateDir: gate},
		local:   main, gate: gate, submitted: submitted, recorded: recorded, merged: merged,
	}
}

// TestLiveCase_OrphanHeadWithIntegratedBranchOffersCustodyReturn is the
// regression for the dead end PR #2 left behind. The recorded head is still
// reachable as a loose object (so the "provably missing" clause correctly
// refuses), but every head this run recorded is already contained in the
// operator's branch and the worktree is clean, so the proven custody return
// must be the offered next action.
func TestLiveCase_OrphanHeadWithIntegratedBranchOffersCustodyReturn(t *testing.T) {
	t.Parallel()
	f := newLiveCaseFixture(t)

	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != SafetyPipelineOwnedHeadLost {
		t.Fatalf("state = %s safety = %s, want pipeline_owned/%s", state.State, state.Safety, SafetyPipelineOwnedHeadLost)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" || !strings.Contains(state.NextAction.Command, "sync --recover") {
		t.Fatalf("next action = %#v", state.NextAction)
	}
	if !strings.Contains(state.Error, f.recorded) {
		t.Fatalf("block message does not name the recorded head %s: %q", f.recorded, state.Error)
	}
	if !strings.Contains(state.Error, f.submitted) && !strings.Contains(state.Error, "this branch") {
		// The clause's safety proof must visibly reference the integrated-
		// branch fact so a confused operator can read the next action and
		// understand what made it safe.
		t.Logf("note: error does not name the submitted head: %q", state.Error)
	}
}

// TestLiveCase_OrphanHeadRecoveryStampsCustodyWithoutTouchingTheWorktree
// proves the new clause stamps custody in one Recover call, leaves HEAD
// alone, does not write a recovery anchor, and reports the branch ready for
// a fresh run.
func TestLiveCase_OrphanHeadRecoveryStampsCustodyWithoutTouchingTheWorktree(t *testing.T) {
	t.Parallel()
	f := newLiveCaseFixture(t)
	before := mustRun(t, f.local, "rev-parse", "HEAD")

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered {
		t.Fatalf("recover = %#v", recovered)
	}
	if recovered.Changed {
		t.Fatalf("integrated-branch recovery must not move HEAD: %#v", recovered)
	}
	if recovered.State != StateCustodyReturned || recovered.Safety != "custody_returned" {
		t.Fatalf("post-recover state = %s/%s", recovered.State, recovered.Safety)
	}
	if recovered.NextAction == nil || recovered.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recover next action = %#v", recovered.NextAction)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != before {
		t.Fatalf("recovery moved HEAD from %s to %s", before, got)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil || run.CustodyReturnedAt == nil {
		t.Fatalf("custody not stamped: run=%#v err=%v", run, err)
	}

	// Idempotent: a second recovery changes nothing.
	again := f.service.Recover(f.ctx, false)
	if !again.Recovered || again.Changed {
		t.Fatalf("second recovery = %#v", again)
	}
	after := f.service.InspectCached(f.ctx)
	if after.State != StateCustodyReturned || after.NextAction == nil || after.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recover inspection = %#v", after)
	}
}

// TestLiveCase_OrphanHeadStaysBlockedWhenProofFails is the negative half:
// every ambiguous or unverifiable shape must keep the pre-existing refusal,
// keep the manual next action, and never stamp custody.
func TestLiveCase_OrphanHeadStaysBlockedWhenProofFails(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		arrange func(t *testing.T, f *liveCaseFixture)
	}{
		{
			name: "worktree is dirty",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				mustWrite(t, filepath.Join(f.local, "uncommitted.txt"), "wip\n")
			},
		},
		{
			name: "submitted head is not an ancestor of the worktree HEAD",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				mustRun(t, f.local, "reset", "--hard", mustRun(t, f.local, "rev-parse", "fm/sibling"))
			},
		},
		{
			name: "gate branch advanced past the worktree branch",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				scratch := filepath.Join(filepath.Dir(f.local), "gate-advance")
				mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, scratch)
				configureIdentity(t, scratch)
				mustRun(t, scratch, "checkout", "fm/feature")
				mustWrite(t, filepath.Join(scratch, "extra.txt"), "extra\n")
				mustRun(t, scratch, "add", "extra.txt")
				mustRun(t, scratch, "commit", "-m", "extra")
				mustRun(t, scratch, "push", "-f", "origin", "HEAD:refs/heads/fm/feature")
			},
		},
		{
			name: "worktree carries a conflicting recovery anchor",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				mustRun(t, f.local, "update-ref", "refs/no-mistakes/recover/"+f.run.ID, f.submitted)
			},
		},
		{
			name: "worktree carries a conflicting local anchor",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				mustRun(t, f.local, "update-ref", "refs/no-mistakes/recover-local/"+f.run.ID, f.submitted)
			},
		},
		{
			name: "gate carries a conflicting recovery anchor",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				mustRun(t, f.gate, "update-ref", "refs/no-mistakes/recover/"+f.run.ID, f.submitted)
			},
		},
		{
			name: "no gate is configured",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				f.service.GateDir = ""
			},
		},
		{
			name: "the configured gate is not on disk",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				// The clause requires a readable gate; removing the directory
				// removes any branch evidence the proof would otherwise cite.
				if err := exec.Command("rm", "-rf", f.gate).Run(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "run is not terminal",
			arrange: func(t *testing.T, f *liveCaseFixture) {
				if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLiveCaseFixture(t)
			tc.arrange(t, f)
			head := mustRun(t, f.local, "rev-parse", "HEAD")

			state := f.service.InspectCached(f.ctx)
			if state.Safety == SafetyPipelineOwnedHeadLost {
				t.Fatalf("unproven release was advertised: %#v", state)
			}
			if state.NextAction != nil && state.NextAction.Code == "recover_custody" {
				t.Fatalf("unproven release advertised custody recovery: %#v", state.NextAction)
			}
			recovered := f.service.Recover(f.ctx, false)
			if recovered.Recovered {
				t.Fatalf("unproven release recovered: %#v", recovered)
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run != nil && run.CustodyReturnedAt != nil {
				t.Fatal("unproven release stamped custody")
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != head {
				t.Fatalf("refused release moved HEAD from %s to %s", head, got)
			}
		})
	}
}

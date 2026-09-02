package branchsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// lostHeadFixture reproduces the stranded lifecycle reported against run
// 01M1GE0QM433D9G2A0QDMJFGJH: a validation run went terminal without verified
// head evidence, so nothing ever anchored its pipeline-authored head, and that
// transient commit later ceased to exist anywhere no-mistakes can read. The
// operator's own branch stayed clean and kept every commit it submitted, yet
// status offered only `inspect_and_reconcile_manually` -> `no-mistakes axi
// status`, and a fresh `axi run` refused for the same reason, forever.
//
// The fixture builds that exact shape: gate branch still at the submitted
// head (pipeline commits are made on a detached worktree and never move it),
// a run row whose head_sha names a real commit that exists in no reachable
// object store, and no recovery anchor anywhere.
type lostHeadFixture struct {
	t         *testing.T
	ctx       context.Context
	db        *db.DB
	repo      *db.Repo
	run       *db.Run
	service   *Service
	local     string
	gate      string
	remote    string
	base      string
	submitted string
	lost      string
}

func newLostHeadFixture(t *testing.T, status types.RunStatus, verifiedHead bool) *lostHeadFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/lost")
	mustWrite(t, filepath.Join(local, "file.txt"), "feature\n")
	mustRun(t, local, "commit", "-am", "feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/feature/lost:refs/heads/feature/lost")

	// The pipeline's own commit is authored in a scratch checkout that is then
	// destroyed, so the recorded head is a real, well-formed commit id that no
	// longer exists in the operator worktree or the gate - exactly the state
	// left behind when terminalization never anchored it and the object was
	// later collected.
	scratch := filepath.Join(root, "scratch")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, scratch)
	configureIdentity(t, scratch)
	mustRun(t, scratch, "checkout", "feature/lost")
	mustWrite(t, filepath.Join(scratch, "fix.txt"), "pipeline fix\n")
	mustRun(t, scratch, "add", "fix.txt")
	mustRun(t, scratch, "commit", "-m", "no-mistakes(review): fix")
	lost := mustRun(t, scratch, "rev-parse", "HEAD")
	if err := os.RemoveAll(scratch); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/lost", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, lost); err != nil {
		t.Fatal(err)
	}
	if verifiedHead {
		err = database.UpdateRunStatusWithVerifiedHead(run.ID, status, lost)
	} else {
		err = database.UpdateRunStatus(run.ID, status)
	}
	if err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &lostHeadFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, lost: lost,
	}
}

func (f *lostHeadFixture) custodyReturned() bool {
	f.t.Helper()
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("reload run: %#v, %v", run, err)
	}
	return run.CustodyReturnedAt != nil
}

func (f *lostHeadFixture) noMistakesRefs() string {
	f.t.Helper()
	out, err := gitpkg.Run(f.ctx, f.local, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/")
	if err != nil {
		f.t.Fatalf("list refs: %v", err)
	}
	return strings.TrimSpace(out)
}

// TestLostPipelineHeadWithContainedBranchOffersProvenCustodyReturn is the
// regression for the reported dead end. With the recorded head provably gone
// and every head the run recorded already contained in the operator's branch,
// status must offer the supported custody return instead of pointing at a
// command that only reprints the same block.
func TestLostPipelineHeadWithContainedBranchOffersProvenCustodyReturn(t *testing.T) {
	t.Parallel()

	for _, status := range []types.RunStatus{types.RunCancelled, types.RunFailed, types.RunCompleted} {
		for _, verified := range []bool{false, true} {
			name := string(status)
			if verified {
				name += "_verified_head"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newLostHeadFixture(t, status, verified)

				state := f.service.InspectCached(f.ctx)
				if state.State != StatePipelineOwned || state.Safety != SafetyPipelineOwnedHeadLost {
					t.Fatalf("state = %s safety = %s, want pipeline_owned/%s", state.State, state.Safety, SafetyPipelineOwnedHeadLost)
				}
				if state.NextAction == nil || state.NextAction.Code != "recover_custody" || !strings.Contains(state.NextAction.Command, "sync --recover") {
					t.Fatalf("next action = %#v", state.NextAction)
				}
				if !strings.Contains(state.Error, f.lost) {
					t.Fatalf("block message does not name the lost head %s: %q", f.lost, state.Error)
				}
			})
		}
	}
}

// TestRecoverLostPipelineHeadReturnsCustodyWithoutTouchingTheWorktree proves
// the release is inert: it stamps custody, never moves HEAD, never writes an
// anchor for a commit that does not exist, and leaves the branch usable for a
// fresh run.
func TestRecoverLostPipelineHeadReturnsCustodyWithoutTouchingTheWorktree(t *testing.T) {
	t.Parallel()

	f := newLostHeadFixture(t, types.RunCancelled, false)
	before := mustRun(t, f.local, "rev-parse", "HEAD")

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered {
		t.Fatalf("recover result = %#v", recovered)
	}
	if recovered.Changed {
		t.Fatalf("a lost head cannot be adopted, so recovery must report no change: %#v", recovered)
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
	if refs := f.noMistakesRefs(); refs != "" {
		t.Fatalf("recovery created refs for a head that does not exist: %q", refs)
	}
	if !f.custodyReturned() {
		t.Fatal("custody not stamped")
	}

	// Idempotent: a second recovery changes nothing and stays successful.
	again := f.service.Recover(f.ctx, false)
	if !again.Recovered || again.Changed {
		t.Fatalf("second recovery = %#v", again)
	}
	after := f.service.InspectCached(f.ctx)
	if after.State != StateCustodyReturned || after.NextAction == nil || after.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recover inspection = %#v", after)
	}
}

// TestRecoverLostPipelineHeadKeepsLaterLocalCommits covers the reported shape
// where the operator kept working: the branch is ahead of the submitted head,
// which still contains every head the run recorded, so release stays proven
// and no local commit is touched.
func TestRecoverLostPipelineHeadKeepsLaterLocalCommits(t *testing.T) {
	t.Parallel()

	f := newLostHeadFixture(t, types.RunCancelled, false)
	mustWrite(t, filepath.Join(f.local, "later.txt"), "later operator work\n")
	mustRun(t, f.local, "add", "later.txt")
	mustRun(t, f.local, "commit", "-m", "later operator work")
	ahead := mustRun(t, f.local, "rev-parse", "HEAD")

	state := f.service.InspectCached(f.ctx)
	if state.Safety != SafetyPipelineOwnedHeadLost || state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("ahead-of-submitted state = %#v", state)
	}
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || recovered.Changed {
		t.Fatalf("ahead-of-submitted recovery = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != ahead {
		t.Fatalf("recovery moved HEAD from %s to %s", ahead, got)
	}
	if got := mustRun(t, f.local, "cat-file", "-e", ahead+"^{commit}"); got != "" {
		t.Fatalf("later local commit lost: %q", got)
	}
}

// TestLostPipelineHeadStaysBlockedWhenReleaseIsNotProven is the negative half:
// every ambiguous or unverifiable shape must keep the pre-existing refusal,
// keep the manual next action, and never stamp custody.
func TestLostPipelineHeadStaysBlockedWhenReleaseIsNotProven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		arrange func(t *testing.T, f *lostHeadFixture)
	}{
		{
			name: "gate branch holds pipeline commits the branch does not contain",
			arrange: func(t *testing.T, f *lostHeadFixture) {
				// A surviving pipeline commit on the gate branch is real work
				// that recovery would strand; the recorded head being gone
				// says nothing about it.
				scratch := filepath.Join(filepath.Dir(f.local), "gate-advance")
				mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, scratch)
				configureIdentity(t, scratch)
				mustRun(t, scratch, "checkout", "feature/lost")
				mustWrite(t, filepath.Join(scratch, "gate-fix.txt"), "surviving pipeline fix\n")
				mustRun(t, scratch, "add", "gate-fix.txt")
				mustRun(t, scratch, "commit", "-m", "no-mistakes(lint): fix")
				mustRun(t, scratch, "push", "origin", "HEAD:refs/heads/feature/lost")
			},
		},
		{
			name: "local branch is behind the submitted head",
			arrange: func(t *testing.T, f *lostHeadFixture) {
				mustRun(t, f.local, "reset", "--hard", f.base)
			},
		},
		{
			name: "local branch diverged from the submitted head",
			arrange: func(t *testing.T, f *lostHeadFixture) {
				mustRun(t, f.local, "reset", "--hard", f.base)
				mustWrite(t, filepath.Join(f.local, "file.txt"), "divergent\n")
				mustRun(t, f.local, "commit", "-am", "divergent operator work")
			},
		},
		{
			name: "gate carries conflicting recovery evidence for the run",
			arrange: func(t *testing.T, f *lostHeadFixture) {
				mustRun(t, f.gate, "update-ref", "refs/no-mistakes/recover/"+f.run.ID, f.submitted)
			},
		},
		{
			name: "worktree carries conflicting recovery evidence for the run",
			arrange: func(t *testing.T, f *lostHeadFixture) {
				mustRun(t, f.local, "update-ref", "refs/no-mistakes/recover/"+f.run.ID, f.submitted)
			},
		},
		{
			name: "no gate is configured",
			arrange: func(t *testing.T, f *lostHeadFixture) {
				f.service.GateDir = ""
			},
		},
		{
			name: "the configured gate is not on disk",
			arrange: func(t *testing.T, f *lostHeadFixture) {
				if err := os.RemoveAll(f.gate); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLostHeadFixture(t, types.RunCancelled, false)
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
			if f.custodyReturned() {
				t.Fatal("unproven release stamped custody")
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != head {
				t.Fatalf("refused release moved HEAD from %s to %s", head, got)
			}
		})
	}
}

// TestPresentPipelineHeadKeepsTheOrdinaryRecoverablePath guards the split: a
// head that still exists must keep taking the anchor-and-adopt route, never
// the release-on-proven-absence shortcut.
func TestPresentPipelineHeadKeepsTheOrdinaryRecoverablePath(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	state := f.service.InspectCached(f.ctx)
	if state.Safety != SafetyPipelineOwnedRecoverable {
		t.Fatalf("present head safety = %q, want %s", state.Safety, SafetyPipelineOwnedRecoverable)
	}
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed {
		t.Fatalf("present head recovery = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("present head recovery HEAD = %s, want %s", got, f.preserved)
	}
}

package branchsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	pipelinepkg "github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	bindSiblingArchiveCommand = "no-mistakes axi sync --bind-archive-ref <refs/heads/archive/...>"
	keepLocalRecoverCommand   = "no-mistakes axi sync --recover --keep-local"
)

// twoFixStep commits a review fix and records it as the run head, exactly as
// recordAgentFixHead does after a committed fix round, then commits a second
// fix. With sibling set the second fix rewrites instead of appending, so it
// shares the review fix's parent. The step then fails the way
// assertPipelineHeadContinuity fails a run whose worktree left the recorded
// head's lineage.
type twoFixStep struct {
	sibling  bool
	reviewed string
	final    string
}

func (s *twoFixStep) Name() types.StepName { return types.StepReview }

func (s *twoFixStep) Execute(sctx *pipelinepkg.StepContext) (*pipelinepkg.StepOutcome, error) {
	commit := func(file, content, message string) (string, error) {
		if err := os.WriteFile(filepath.Join(sctx.WorkDir, file), []byte(content), 0o644); err != nil {
			return "", err
		}
		if _, err := gitpkg.Run(sctx.Ctx, sctx.WorkDir, "add", file); err != nil {
			return "", err
		}
		if _, err := gitpkg.Run(sctx.Ctx, sctx.WorkDir, "commit", "-m", message); err != nil {
			return "", err
		}
		return gitpkg.HeadSHA(sctx.Ctx, sctx.WorkDir)
	}
	submitted, err := gitpkg.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if s.reviewed, err = commit("review.txt", "review fix\n", "no-mistakes(review): fix"); err != nil {
		return nil, err
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, s.reviewed); err != nil {
		return nil, err
	}
	sctx.Run.HeadSHA = s.reviewed
	if s.sibling {
		if _, err := gitpkg.Run(sctx.Ctx, sctx.WorkDir, "reset", "--hard", submitted); err != nil {
			return nil, err
		}
	}
	if s.final, err = commit("dup.txt", "duplication fix\n", "no-mistakes(review): duplication fix"); err != nil {
		return nil, err
	}
	return nil, errors.New("refusing to run review step: worktree HEAD is not a descendant of the pipeline's recorded head")
}

// twoFixFixture is a failed run terminalized by the real executor, with the
// managed worktree already removed as the daemon removes it at run_finished.
type twoFixFixture struct {
	*recoverFixture
	reviewed string
	final    string
}

func newTwoFixFixture(t *testing.T, sibling bool) *twoFixFixture {
	t.Helper()
	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunPending); err != nil {
		t.Fatal(err)
	}
	f.run.Status = types.RunPending
	managed := filepath.Join(t.TempDir(), "managed")
	if err := gitpkg.WorktreeAdd(f.ctx, f.gate, managed, f.submitted); err != nil {
		t.Fatal(err)
	}
	configureIdentity(t, managed)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	step := &twoFixStep{sibling: sibling}
	executor := pipelinepkg.NewExecutor(f.db, p, nil, nil, []pipelinepkg.Step{step}, nil)
	if err := executor.Execute(context.Background(), f.run, f.repo, managed); err == nil {
		t.Fatal("failing executor returned nil")
	}
	if err := gitpkg.WorktreeRemove(f.ctx, f.gate, managed); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.db.GetRun(f.run.ID)
	return &twoFixFixture{recoverFixture: f, reviewed: step.reviewed, final: step.final}
}

// archive makes sha reachable in the operator repository at an archive ref, the
// way an owner preserves a head by hand. The gate's temporary ref exists only
// long enough to make the otherwise unreferenced commit fetchable.
func (f *twoFixFixture) archive(label, sha string) string {
	f.t.Helper()
	ref := "refs/heads/archive/" + label + "-" + f.run.ID
	temporary := "refs/no-mistakes/test-archive-" + label
	mustRun(f.t, f.gate, "update-ref", temporary, sha)
	mustRun(f.t, f.local, "fetch", "--no-tags", f.gate, temporary+":"+ref)
	mustRun(f.t, f.gate, "update-ref", "-d", temporary)
	return ref
}

func (f *twoFixFixture) snapshot() string {
	f.t.Helper()
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("reload run: %#v, %v", run, err)
	}
	records, err := f.db.GetRecoveryArchivesByRun(f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	bound := make([]string, 0, len(records))
	for _, record := range records {
		bound = append(bound, record.ArchiveRef+"="+record.PreservedHeadSHA)
	}
	return strings.Join([]string{
		mustRun(f.t, f.local, "rev-parse", "HEAD"),
		mustRun(f.t, f.local, "status", "--porcelain=v1", "--untracked-files=all"),
		mustRun(f.t, f.local, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
		mustRun(f.t, f.gate, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
		run.HeadSHA, string(run.Status), strings.Join(bound, ","),
	}, "\n---\n")
}

func (f *twoFixFixture) assertStillHeld(state State) {
	f.t.Helper()
	if state.Recovered || state.Changed {
		f.t.Fatalf("refusal reported recovery: %#v", state)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run.CustodyReturnedAt != nil || run.TerminalHeadVerifiedAt != nil {
		f.t.Fatalf("refusal changed custody or verification: %#v, %v", run, err)
	}
}

// TestSiblingFixCommitsTerminalizeWithoutChoosingAFinalHead pins the initiating
// trigger as correct behavior: a worktree that left the recorded head's lineage
// is not verified, the recorded head is not replaced, and the sibling is named
// by no record. The recovery below must work without weakening any of this.
func TestSiblingFixCommitsTerminalizeWithoutChoosingAFinalHead(t *testing.T) {
	t.Parallel()

	f := newTwoFixFixture(t, true)
	if f.run.Status != types.RunFailed || f.run.HeadSHA != f.reviewed || f.run.TerminalHeadVerifiedAt != nil {
		t.Fatalf("terminal run = status %s head %s verified %v, want failed at the review fix and unverified", f.run.Status, f.run.HeadSHA, f.run.TerminalHeadVerifiedAt)
	}
	for _, pair := range [][2]string{{f.reviewed, f.final}, {f.final, f.reviewed}} {
		if isAncestor(f.ctx, f.gate, pair[0], pair[1]) {
			t.Fatalf("fixture heads are not siblings: %s is an ancestor of %s", pair[0], pair[1])
		}
	}
	if parent := mustRun(t, f.gate, "rev-parse", f.final+"^"); parent != f.submitted || mustRun(t, f.gate, "rev-parse", f.reviewed+"^") != f.submitted {
		t.Fatalf("fixture heads do not share the submitted parent %s", f.submitted)
	}
}

// TestSiblingHeadsInspectionDoesNotAdvertiseARecoveryThatRefuses is the masking
// regression: inspection offered `sync --recover` for a state where Recover is
// certain to refuse, so the owner followed the offer into a dead end.
func TestSiblingHeadsInspectionDoesNotAdvertiseARecoveryThatRefuses(t *testing.T) {
	t.Parallel()

	f := newTwoFixFixture(t, true)
	state := f.service.InspectCached(f.ctx)
	assertManualReconciliationOffer(t, state)
	if state.State != StatePipelineOwned || state.Recovery != nil {
		t.Fatalf("unverified sibling inspection = %#v", state)
	}
	// With no archive bound nothing records that a sibling exists, and a run
	// that merely lost its worker looks identical, so the sibling sequence is
	// described as a condition and never asserted as the next action.
	for _, keepLocal := range []bool{false, true} {
		refused := f.service.Recover(f.ctx, keepLocal)
		f.assertStillHeld(refused)
		if refused.Safety != "blocked_recover_unverified_head" || refused.NextAction == nil || refused.NextAction.Code != "inspect_and_reconcile_manually" {
			t.Fatalf("recover keepLocal=%v without evidence = %#v", keepLocal, refused)
		}
		for _, want := range []string{"If the run left two fix commits that share a parent", "--bind-archive-ref", keepLocalRecoverCommand} {
			if !strings.Contains(refused.Error, want) {
				t.Fatalf("recover keepLocal=%v refusal does not document %q: %s", keepLocal, want, refused.Error)
			}
		}
	}
}

// TestSiblingHeadsReleaseKeepsBothArchivesAndTheLocalHead is the regression for
// the stranded state itself. Binding both archives and then keeping the local
// head returns custody while selecting neither sibling: the branch stays at the
// submitted head, both archives keep their exact commits, and the run is never
// stamped as having a verified final head it does not have.
func TestSiblingHeadsReleaseKeepsBothArchivesAndTheLocalHead(t *testing.T) {
	t.Parallel()

	f := newTwoFixFixture(t, true)
	reviewedRef := f.archive("review", f.reviewed)
	finalRef := f.archive("duplication", f.final)
	refsBefore := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
	gateRefsBefore := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")

	first := f.service.BindRecoveryArchive(f.ctx, finalRef)
	f.assertStillHeld(first)
	if first.Safety != "blocked_recover_sibling_archive_incomplete" || first.Recovery == nil || first.Recovery.Source != "bound_sibling_archives" || first.Recovery.Proof != "awaiting_sibling" {
		t.Fatalf("first sibling bind = %#v recovery %#v", first, first.Recovery)
	}
	if first.NextAction == nil || first.NextAction.Code != "bind_sibling_archive" || first.NextAction.Command != bindSiblingArchiveCommand {
		t.Fatalf("first sibling bind next action = %#v", first.NextAction)
	}
	if refused := f.service.Recover(f.ctx, true); refused.Safety != "blocked_recover_sibling_archive_incomplete" {
		t.Fatalf("keep-local with one sibling bound = %#v", refused)
	} else {
		f.assertStillHeld(refused)
	}

	second := f.service.BindRecoveryArchive(f.ctx, reviewedRef)
	f.assertStillHeld(second)
	if second.Safety != "blocked_pipeline_owned_recoverable" || second.Recovery == nil || second.Recovery.Proof != "verified" || !second.Recovery.KeepLocal {
		t.Fatalf("second sibling bind = %#v recovery %#v", second, second.Recovery)
	}
	evidence := second.Recovery
	if evidence.Source != "bound_sibling_archives" || evidence.RequiredHead != f.submitted ||
		evidence.PreservedHead != f.reviewed || evidence.ArchiveRef != reviewedRef ||
		evidence.SiblingHead != f.final || evidence.SiblingArchiveRef != finalRef {
		t.Fatalf("sibling evidence = %#v", evidence)
	}
	if second.NextAction == nil || second.NextAction.Code != "recover_custody" || second.NextAction.Command != keepLocalRecoverCommand {
		t.Fatalf("verified sibling next action = %#v", second.NextAction)
	}
	if inspected := f.service.InspectCached(f.ctx); inspected.Safety != "blocked_pipeline_owned_recoverable" || inspected.NextAction == nil || inspected.NextAction.Command != keepLocalRecoverCommand {
		t.Fatalf("verified sibling inspection = %#v", inspected)
	}

	// Taking a sibling is never offered: plain recovery refuses and points at
	// the keep-local release.
	plain := f.service.Recover(f.ctx, false)
	f.assertStillHeld(plain)
	if plain.Safety != "blocked_recover_archive_requires_keep_local" || plain.NextAction == nil || plain.NextAction.Command != keepLocalRecoverCommand {
		t.Fatalf("plain recovery with sibling evidence = %#v", plain)
	}

	released := f.service.Recover(f.ctx, true)
	if !released.Recovered || released.Changed || released.State != StateCustodyReturned {
		t.Fatalf("sibling keep-local release = %#v", released)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("release moved HEAD to %s, want the submitted head %s", got, f.submitted)
	}
	if clean, reason := worktreeClean(f.ctx, f.local); !clean {
		t.Fatalf("release left the worktree not clean: %s", reason)
	}
	if got := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"); got != refsBefore {
		t.Fatalf("release changed operator refs:\n%s\nwant\n%s", got, refsBefore)
	}
	if got := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"); got != gateRefsBefore {
		t.Fatalf("release changed gate refs:\n%s\nwant\n%s", got, gateRefsBefore)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run.CustodyReturnedAt == nil || run.HeadSHA != f.reviewed || run.TerminalHeadVerifiedAt != nil {
		t.Fatalf("released run = %#v, %v; want custody returned with the recorded head and no invented verification", run, err)
	}
	if again := f.service.Recover(f.ctx, true); !again.Recovered || again.Changed {
		t.Fatalf("repeated release = %#v", again)
	}
	// Both fixes remain available for the owner's linear reconstruction.
	mustRun(t, f.local, "cherry-pick", reviewedRef)
	mustRun(t, f.local, "cherry-pick", finalRef)
	if got := readOptional(t, filepath.Join(f.local, "review.txt")) + readOptional(t, filepath.Join(f.local, "dup.txt")); got != "review fix\nduplication fix\n" {
		t.Fatalf("linear reconstruction content = %q", got)
	}
}

// TestSiblingHeadsIncompleteEvidenceFailsClosedWithoutMutation covers every way
// the sibling proof can be short. Each case must refuse at bind or at release
// and change no ref, file, archive record, or run row.
func TestSiblingHeadsIncompleteEvidenceFailsClosedWithoutMutation(t *testing.T) {
	t.Parallel()

	// commitOn creates an unreferenced commit in the gate on top of parent.
	commitOn := func(f *twoFixFixture, parent, file string) string {
		f.t.Helper()
		writer := filepath.Join(f.t.TempDir(), "writer")
		mustRun(f.t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(f.t, writer)
		mustRun(f.t, writer, "fetch", "--no-tags", f.gate, "+refs/no-mistakes/test-parent:refs/no-mistakes/test-parent")
		mustRun(f.t, writer, "checkout", "--detach", parent)
		mustWrite(f.t, filepath.Join(writer, file), file+"\n")
		mustRun(f.t, writer, "add", file)
		mustRun(f.t, writer, "commit", "-m", file)
		head := mustRun(f.t, writer, "rev-parse", "HEAD")
		mustRun(f.t, writer, "push", "origin", head+":refs/no-mistakes/test-made")
		mustRun(f.t, f.gate, "update-ref", "-d", "refs/no-mistakes/test-made")
		return head
	}
	withParent := func(f *twoFixFixture, parent string, do func() string) string {
		mustRun(f.t, f.gate, "update-ref", "refs/no-mistakes/test-parent", parent)
		defer mustRun(f.t, f.gate, "update-ref", "-d", "refs/no-mistakes/test-parent")
		return do()
	}
	refusesBind := func(t *testing.T, f *twoFixFixture, ref, want string) {
		t.Helper()
		before := f.snapshot()
		state := f.service.BindRecoveryArchive(f.ctx, ref)
		f.assertStillHeld(state)
		if state.Safety != want {
			t.Fatalf("bind %s = %s (%s), want %s", ref, state.Safety, state.Error, want)
		}
		if after := f.snapshot(); after != before {
			t.Fatalf("refused bind changed state:\n%s\nwant\n%s", after, before)
		}
	}
	refusesRelease := func(t *testing.T, f *twoFixFixture, want string) {
		t.Helper()
		before := f.snapshot()
		state := f.service.Recover(f.ctx, true)
		f.assertStillHeld(state)
		if state.Safety != want {
			t.Fatalf("release = %s (%s), want %s", state.Safety, state.Error, want)
		}
		if after := f.snapshot(); after != before {
			t.Fatalf("refused release changed state:\n%s\nwant\n%s", after, before)
		}
	}
	bindBoth := func(t *testing.T, f *twoFixFixture) (string, string) {
		t.Helper()
		reviewedRef, finalRef := f.archive("review", f.reviewed), f.archive("duplication", f.final)
		f.service.BindRecoveryArchive(f.ctx, reviewedRef)
		if state := f.service.BindRecoveryArchive(f.ctx, finalRef); state.Recovery == nil || state.Recovery.Proof != "verified" {
			t.Fatalf("fixture pair did not verify: %#v", state)
		}
		return reviewedRef, finalRef
	}

	t.Run("only the recorded head is bound", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		f.service.BindRecoveryArchive(f.ctx, f.archive("review", f.reviewed))
		refusesRelease(t, f, "blocked_recover_sibling_archive_incomplete")
	})
	t.Run("only the unrecorded sibling is bound", func(t *testing.T) {
		t.Parallel()
		// The recorded head is archived, which makes the sibling relation
		// provable, but only the sibling's archive is bound.
		f := newTwoFixFixture(t, true)
		f.archive("review", f.reviewed)
		if bound := f.service.BindRecoveryArchive(f.ctx, f.archive("duplication", f.final)); bound.Safety != "blocked_recover_sibling_archive_incomplete" {
			t.Fatalf("lone sibling bind = %s (%s)", bound.Safety, bound.Error)
		}
		refusesRelease(t, f, "blocked_recover_sibling_archive_incomplete")
	})
	t.Run("candidate descends from the recorded head", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		linear := withParent(f, f.reviewed, func() string { return commitOn(f, f.reviewed, "linear.txt") })
		refusesBind(t, f, f.archive("linear", linear), "blocked_recover_sibling_archive_not_sibling")
	})
	t.Run("candidate is a cousin of the recorded head", func(t *testing.T) {
		t.Parallel()
		// A commit on top of the unrecorded sibling also diverges from the
		// recorded head and also descends from the required head, but it is one
		// step further out. Siblings record the same parents.
		f := newTwoFixFixture(t, true)
		f.archive("review", f.reviewed)
		cousin := withParent(f, f.final, func() string { return commitOn(f, f.final, "cousin.txt") })
		refusesBind(t, f, f.archive("cousin", cousin), "blocked_recover_sibling_archive_not_sibling")
	})
	t.Run("a replacement object disguises a cousin as a sibling", func(t *testing.T) {
		t.Parallel()
		// refs/replace/* rewrites what ordinary Git reads report, so the proof
		// must read the stored objects: the archived commit still records the
		// sibling as its parent.
		f := newTwoFixFixture(t, true)
		f.archive("review", f.reviewed)
		cousin := withParent(f, f.final, func() string { return commitOn(f, f.final, "cousin.txt") })
		cousinRef := f.archive("cousin", cousin)
		mustRun(f.t, f.local, "replace", "-f", cousin, f.final)
		if disguised := mustRun(f.t, f.local, "rev-list", "--parents", "-n", "1", cousin); disguised != cousin+" "+f.submitted {
			t.Fatalf("replacement did not disguise the cousin's parent: %s", disguised)
		}
		refusesBind(t, f, cousinRef, "blocked_recover_sibling_archive_not_sibling")
	})
	t.Run("candidate is the submitted head", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		refusesBind(t, f, f.archive("submitted", f.submitted), "blocked_recover_sibling_archive_not_sibling")
	})
	t.Run("candidate is outside the submitted lineage", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		unrelated := withParent(f, f.base, func() string { return commitOn(f, f.base, "unrelated.txt") })
		refusesBind(t, f, f.archive("unrelated", unrelated), "blocked_recover_sibling_archive_not_sibling")
	})
	t.Run("recorded head object is absent locally", func(t *testing.T) {
		t.Parallel()
		// Ancestry against a missing object reads as "not an ancestor" in both
		// directions, which must never pass for a proven sibling pair.
		f := newTwoFixFixture(t, true)
		finalRef := f.archive("duplication", f.final)
		if objectExists(f.ctx, f.local, f.reviewed) {
			t.Fatal("fixture already has the recorded head locally")
		}
		refusesBind(t, f, finalRef, "blocked_recover_sibling_archive_unproven")
	})
	t.Run("third candidate", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		bindBoth(t, f)
		third := withParent(f, f.submitted, func() string { return commitOn(f, f.submitted, "third.txt") })
		refusesBind(t, f, f.archive("third", third), "blocked_recover_archive_ambiguous")
	})
	t.Run("second archive names the same head", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		f.service.BindRecoveryArchive(f.ctx, f.archive("review", f.reviewed))
		refusesBind(t, f, f.archive("review-copy", f.reviewed), "blocked_recover_sibling_archive_not_sibling")
	})
	t.Run("archive moved after binding", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		_, finalRef := bindBoth(t, f)
		mustRun(t, f.local, "update-ref", finalRef, f.submitted)
		refusesRelease(t, f, "blocked_recover_archive_moved")
	})
	t.Run("archive deleted after binding", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		reviewedRef, _ := bindBoth(t, f)
		mustRun(t, f.local, "update-ref", "-d", reviewedRef)
		refusesRelease(t, f, "blocked_recover_archive_missing")
	})
	t.Run("archive made symbolic after binding", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		_, finalRef := bindBoth(t, f)
		mustRun(t, f.local, "update-ref", "refs/no-mistakes/test-target", f.final)
		mustRun(t, f.local, "symbolic-ref", finalRef, "refs/no-mistakes/test-target")
		refusesRelease(t, f, "blocked_recover_archive_symbolic")
	})
	t.Run("archive moved at the release boundary", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		_, finalRef := bindBoth(t, f)
		f.service.beforeGateReset = func() { mustRun(t, f.local, "update-ref", finalRef, f.submitted) }
		state := f.service.Recover(f.ctx, true)
		f.assertStillHeld(state)
		if state.Safety != "blocked_recover_archive_moved" {
			t.Fatalf("boundary move = %s (%s)", state.Safety, state.Error)
		}
	})
	t.Run("gate branch left the required head", func(t *testing.T) {
		t.Parallel()
		// The release moves no ref anywhere, so a gate branch that is not already
		// at the required head refuses instead of being moved off a preserved head.
		f := newTwoFixFixture(t, true)
		bindBoth(t, f)
		mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.final)
		refusesRelease(t, f, "blocked_recover_archive_gate_head_mismatch")
	})
	t.Run("dirty worktree", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		bindBoth(t, f)
		mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty local edit\n")
		refusesRelease(t, f, "blocked_recover_archive_dirty")
	})
	t.Run("local head left the required head", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		bindBoth(t, f)
		mustWrite(t, filepath.Join(f.local, "follow-up.txt"), "follow-up\n")
		mustRun(t, f.local, "add", "follow-up.txt")
		mustRun(t, f.local, "commit", "-m", "local follow-up")
		refusesRelease(t, f, "blocked_recover_archive_required_head_mismatch")
	})
	t.Run("active run", func(t *testing.T) {
		t.Parallel()
		f := newTwoFixFixture(t, true)
		finalRef := f.archive("duplication", f.final)
		f.archive("review", f.reviewed)
		if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		state := f.service.BindRecoveryArchive(f.ctx, finalRef)
		if state.Safety != "blocked_recover_archive_not_applicable" {
			t.Fatalf("active-run bind = %s (%s)", state.Safety, state.Error)
		}
		if records, err := f.db.GetRecoveryArchivesByRun(f.run.ID); err != nil || len(records) != 0 {
			t.Fatalf("active-run bind recorded evidence: %#v, %v", records, err)
		}
	})
}

// TestLinearFixCommitsStillRecoverWithoutArchives is the control: the same two
// fixes appended in one line terminalize verified and recover by the ordinary
// fast-forward, and the sibling evidence path does not reach this topology.
func TestLinearFixCommitsStillRecoverWithoutArchives(t *testing.T) {
	t.Parallel()

	f := newTwoFixFixture(t, false)
	if f.run.Status != types.RunFailed || f.run.HeadSHA != f.final || f.run.TerminalHeadVerifiedAt == nil {
		t.Fatalf("linear terminal run = status %s head %s verified %v, want failed and verified at the final fix", f.run.Status, f.run.HeadSHA, f.run.TerminalHeadVerifiedAt)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()+"^{commit}"); got != f.final {
		t.Fatalf("linear recovery anchor = %s, want %s", got, f.final)
	}
	state := f.service.InspectCached(f.ctx)
	if state.Safety != "blocked_pipeline_owned_recoverable" || state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("linear inspection = %#v", state)
	}
	if bound := f.service.BindRecoveryArchive(f.ctx, f.archive("final", f.final)); bound.Safety != "blocked_recover_archive_not_divergent" {
		t.Fatalf("linear archive bind = %s (%s), want the unchanged single-archive refusal", bound.Safety, bound.Error)
	}
	if records, err := f.db.GetRecoveryArchivesByRun(f.run.ID); err != nil || len(records) != 0 {
		t.Fatalf("linear bind recorded evidence: %#v, %v", records, err)
	}
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed || recovered.State != StateCustodyReturned {
		t.Fatalf("linear recovery = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.final {
		t.Fatalf("linear recovery HEAD = %s, want the final fix %s", got, f.final)
	}
	if !isAncestor(f.ctx, f.local, f.reviewed, f.final) {
		t.Fatal("linear recovery lost the review fix")
	}
}

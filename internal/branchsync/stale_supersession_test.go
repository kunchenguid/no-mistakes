package branchsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// staleSupersessionFixture models the production custody chain behind PR 3838:
// an old terminal run retains a recoverable unpublished head, an intermediate
// run submits that exact head and publishes its rebased replacement, and a later
// run submits the operator's exact local head and publishes the exact gate and
// remote head. The old and later heads deliberately have no ancestry relation.
// Git-backed supersession fixtures are process-heavy under -race. Bound this
// file to one fixture at a time so it does not amplify the package-wide Git
// process fan-out and starve unrelated safety tests.
var staleSupersessionFixtureSlot = make(chan struct{}, 1)

type staleSupersessionFixture struct {
	t              *testing.T
	ctx            context.Context
	db             *db.DB
	repo           *db.Repo
	service        *Service
	local          string
	gate           string
	remote         string
	branch         string
	oldRun         *db.Run
	lineageRun     *db.Run
	laterRun       *db.Run
	oldSubmitted   string
	oldPreserved   string
	lineagePushed  string
	laterSubmitted string
	laterPushed    string
}

func newStaleSupersessionFixture(t *testing.T) *staleSupersessionFixture {
	t.Helper()
	staleSupersessionFixtureSlot <- struct{}{}
	t.Cleanup(func() { <-staleSupersessionFixtureSlot })
	ctx := context.Background()
	root := t.TempDir()
	branch := "feature/recover"
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", branch)
	mustWrite(t, filepath.Join(local, "feature.txt"), "operator submission\n")
	mustRun(t, local, "add", "feature.txt")
	mustRun(t, local, "commit", "-m", "operator submission")
	oldSubmitted := mustRun(t, local, "rev-parse", "HEAD")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/"+branch+":refs/heads/"+branch)
	mustRun(t, local, "push", remote, "refs/heads/main:refs/heads/main")
	mustRun(t, local, "remote", "add", "origin", remote)

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	oldRun, err := database.InsertRun(repo.ID, branch, oldSubmitted, base)
	if err != nil {
		t.Fatal(err)
	}

	oldPipeline := filepath.Join(root, "old-pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, oldPipeline)
	configureIdentity(t, oldPipeline)
	mustRun(t, oldPipeline, "checkout", branch)
	mustWrite(t, filepath.Join(oldPipeline, "old-fix.txt"), "old pipeline fix\n")
	mustRun(t, oldPipeline, "add", "old-fix.txt")
	mustRun(t, oldPipeline, "commit", "-m", "no-mistakes(review): old pipeline fix")
	oldPreserved := mustRun(t, oldPipeline, "rev-parse", "HEAD")
	mustRun(t, oldPipeline, "push", "origin", "HEAD:refs/heads/"+branch)
	if err := database.UpdateRunStatusWithVerifiedHead(oldRun.ID, types.RunFailed, oldPreserved); err != nil {
		t.Fatal(err)
	}
	oldRun, _ = database.GetRun(oldRun.ID)
	mustRun(t, local, "fetch", gate, oldPreserved+":refs/no-mistakes/recover/"+oldRun.ID)

	// Advance main, then rebase the old preserved head in the next exact run.
	// This makes oldPreserved and every later branch head siblings, matching the
	// real incident where ancestry cannot prove supersession.
	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "upstream.txt"), "upstream advance\n")
	mustRun(t, local, "add", "upstream.txt")
	mustRun(t, local, "commit", "-m", "upstream advance")
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main")
	mustRun(t, local, "push", remote, "refs/heads/main:refs/heads/main")
	mustRun(t, local, "checkout", branch)

	lineageRun, err := database.InsertRun(repo.ID, branch, oldPreserved, base)
	if err != nil {
		t.Fatal(err)
	}
	lineagePipeline := filepath.Join(root, "lineage-pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, lineagePipeline)
	configureIdentity(t, lineagePipeline)
	mustRun(t, lineagePipeline, "checkout", branch)
	mustRun(t, lineagePipeline, "rebase", "origin/main")
	lineagePushed := mustRun(t, lineagePipeline, "rev-parse", "HEAD")
	mustRun(t, lineagePipeline, "push", "--force", "origin", "HEAD:refs/heads/"+branch)
	mustRun(t, lineagePipeline, "push", "origin", "HEAD:refs/heads/"+branch)
	mustRun(t, lineagePipeline, "push", remote, "HEAD:refs/heads/"+branch)
	recordExactPushedRun(t, database, repo, lineageRun, lineagePushed, branch, types.RunCancelled)
	lineageRun, _ = database.GetRun(lineageRun.ID)

	mustRun(t, local, "fetch", gate, "+refs/heads/"+branch+":refs/no-mistakes/test/lineage")
	mustRun(t, local, "reset", "--hard", lineagePushed)
	mustWrite(t, filepath.Join(local, "operator-later.txt"), "later operator work\n")
	mustRun(t, local, "add", "operator-later.txt")
	mustRun(t, local, "commit", "-m", "later exact submission")
	laterSubmitted := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "push", gate, "HEAD:refs/heads/"+branch)

	laterRun, err := database.InsertRun(repo.ID, branch, laterSubmitted, lineagePushed)
	if err != nil {
		t.Fatal(err)
	}
	laterPipeline := filepath.Join(root, "later-pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, laterPipeline)
	configureIdentity(t, laterPipeline)
	mustRun(t, laterPipeline, "checkout", branch)
	mustWrite(t, filepath.Join(laterPipeline, "later-fix.txt"), "later pipeline fix\n")
	mustRun(t, laterPipeline, "add", "later-fix.txt")
	mustRun(t, laterPipeline, "commit", "-m", "no-mistakes(review): later fix")
	laterPushed := mustRun(t, laterPipeline, "rev-parse", "HEAD")
	mustRun(t, laterPipeline, "push", "origin", "HEAD:refs/heads/"+branch)
	mustRun(t, laterPipeline, "push", remote, "HEAD:refs/heads/"+branch)
	recordExactPushedRun(t, database, repo, laterRun, laterPushed, branch, types.RunFailed)
	laterRun, _ = database.GetRun(laterRun.ID)

	if isAncestor(ctx, gate, oldPreserved, laterPushed) || isAncestor(ctx, gate, laterPushed, oldPreserved) {
		t.Fatal("fixture accidentally made the stale and later heads ancestor-related")
	}
	if !isAncestor(ctx, gate, lineagePushed, laterSubmitted) {
		t.Fatal("fixture omitted the exact later lineage")
	}
	if got := mustRun(t, local, "rev-parse", "HEAD"); got != laterSubmitted {
		t.Fatalf("local head = %s, want exact later submission %s", got, laterSubmitted)
	}
	if got := mustRun(t, gate, "rev-parse", "refs/heads/"+branch); got != laterPushed {
		t.Fatalf("gate head = %s, want exact later push %s", got, laterPushed)
	}
	if got := mustRun(t, remote, "rev-parse", "refs/heads/"+branch); got != laterPushed {
		t.Fatalf("remote head = %s, want exact later push %s", got, laterPushed)
	}

	return &staleSupersessionFixture{
		t: t, ctx: ctx, db: database, repo: repo,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote, branch: branch,
		oldRun: oldRun, lineageRun: lineageRun, laterRun: laterRun,
		oldSubmitted: oldSubmitted, oldPreserved: oldPreserved,
		lineagePushed: lineagePushed, laterSubmitted: laterSubmitted, laterPushed: laterPushed,
	}
}

func recordExactPushedRun(t *testing.T, database *db.DB, repo *db.Repo, run *db.Run, pushed, branch string, status types.RunStatus) {
	t.Helper()
	if err := database.UpdateRunHeadSHA(run.ID, pushed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{
		HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(repo.PushURL()), Ref: "refs/heads/" + branch,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, status, pushed); err != nil {
		t.Fatal(err)
	}
}

func (f *staleSupersessionFixture) assertHeads(t *testing.T, local, gate, remote string) {
	t.Helper()
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != local {
		t.Fatalf("local HEAD = %s, want %s", got, local)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/"+f.branch); got != gate {
		t.Fatalf("gate head = %s, want %s", got, gate)
	}
	if got := mustRun(t, f.remote, "rev-parse", "refs/heads/"+f.branch); got != remote {
		t.Fatalf("remote head = %s, want %s", got, remote)
	}
}

func (f *staleSupersessionFixture) assertOldCustody(t *testing.T, returned bool) {
	t.Helper()
	stored, err := f.db.GetRun(f.oldRun.ID)
	if err != nil || stored == nil {
		t.Fatalf("old run = %#v, %v", stored, err)
	}
	if (stored.CustodyReturnedAt != nil) != returned {
		t.Fatalf("old run custody returned = %v, want %v", stored.CustodyReturnedAt != nil, returned)
	}
	if returned && (stored.CustodyReturnReason == nil || *stored.CustodyReturnReason != db.CustodyReturnReasonStaleOwnerSuperseded) {
		t.Fatalf("old run custody reason = %#v", stored.CustodyReturnReason)
	}
}

func TestPlanStaleCustodyLeavesOrdinaryCustodyPathsUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("ordinary recoverable run", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		before := f.service.InspectCached(f.ctx)
		planned := f.service.PlanStaleCustody(f.ctx)
		if planned.State != before.State || planned.Safety != before.Safety || planned.Pipeline.RunID != before.Pipeline.RunID || planned.NextAction == nil || planned.NextAction.Code != "recover_custody" {
			t.Fatalf("ordinary recovery changed by stale plan: before=%#v after=%#v", before, planned)
		}
		recovered := f.service.Recover(f.ctx, false)
		if !recovered.Recovered || recovered.Local.Head != f.preserved {
			t.Fatalf("ordinary recovery no longer works: %#v", recovered)
		}
	})

	t.Run("unavailable-head release remains separate", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		planned := f.service.PlanStaleCustody(f.ctx)
		if planned.Safety != "blocked_pipeline_owned_recoverable" || planned.NextAction == nil || planned.NextAction.Code != "recover_custody" {
			t.Fatalf("stale plan bypassed unavailable-head recovery: %#v", planned)
		}
		ordinary := f.service.Recover(f.ctx, false)
		if ordinary.NextAction == nil || ordinary.NextAction.Code != "release_unavailable_custody" {
			t.Fatalf("ordinary unavailable release no longer offered: %#v", ordinary)
		}
	})
}

// TestPlanAndSupersedeStaleCustodyExactRunSuccession is the behavior-first
// regression for the PR 3838 incident. It proves the read-only plan binds the
// stale owner and later exact run, then the supported transition anchors every
// old/local/gate/remote head, adopts submitted to pushed without content
// equivalence, and stamps only the old run.
func TestPlanAndSupersedeStaleCustodyExactRunSuccession(t *testing.T) {
	t.Parallel()
	f := newStaleSupersessionFixture(t)

	beforeRefs := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname)")
	plan := f.service.PlanStaleCustody(f.ctx)
	if plan.Released || plan.Changed || plan.Safety != "safe_stale_custody_supersession" {
		t.Fatalf("read-only plan = %#v", plan)
	}
	if plan.NextAction == nil || plan.NextAction.Code != "supersede_stale_custody" ||
		!strings.Contains(plan.NextAction.Command, "--run "+f.oldRun.ID) ||
		!strings.Contains(plan.NextAction.Command, "--later-run "+f.laterRun.ID) {
		t.Fatalf("read-only next action = %#v", plan.NextAction)
	}
	if got := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname)"); got != beforeRefs {
		t.Fatalf("read-only plan mutated refs:\nbefore:\n%s\nafter:\n%s", beforeRefs, got)
	}
	f.assertHeads(t, f.laterSubmitted, f.laterPushed, f.laterPushed)
	f.assertOldCustody(t, false)

	state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	if !state.Released || !state.Changed || state.State != StateSynchronized || state.Safety != "already_synchronized" {
		t.Fatalf("stale custody transition = %#v", state)
	}
	transition := state.CustodyTransition
	if transition == nil || transition.Action != "supersede_stale" || transition.Reason != db.CustodyReturnReasonStaleOwnerSuperseded || transition.Idempotent {
		t.Fatalf("transition = %#v", transition)
	}
	if transition.RunID != f.oldRun.ID || transition.SupersedingRunID != f.laterRun.ID || transition.LineageRunID != f.lineageRun.ID ||
		transition.PreservedHead != f.oldPreserved || transition.SubmittedHead != f.laterSubmitted || transition.PushedHead != f.laterPushed ||
		transition.LocalHead != f.laterSubmitted || transition.RemoteHead != f.laterPushed || transition.GateHead != f.laterPushed {
		t.Fatalf("transition identities = %#v", transition)
	}
	for _, anchor := range []struct {
		ref, dir, want string
	}{
		{transition.PreservedLocalAnchor, f.local, f.oldPreserved},
		{transition.PreservedGateAnchor, f.gate, f.oldPreserved},
		{transition.LocalAnchor, f.local, f.laterSubmitted},
		{transition.RemoteAnchor, f.local, f.laterPushed},
		{transition.GateAnchor, f.gate, f.laterPushed},
	} {
		if anchor.ref == "" {
			t.Fatalf("missing anchor for %s in transition %#v", anchor.want, transition)
		}
		if got := mustRun(t, anchor.dir, "rev-parse", anchor.ref+"^{commit}"); got != anchor.want {
			t.Fatalf("anchor %s = %s, want %s", anchor.ref, got, anchor.want)
		}
	}
	f.assertHeads(t, f.laterPushed, f.laterPushed, f.laterPushed)
	f.assertOldCustody(t, true)
	if later, err := f.db.GetRun(f.laterRun.ID); err != nil || later == nil || later.CustodyReturnedAt != nil {
		t.Fatalf("transition touched later run custody: %#v, %v", later, err)
	}

	retry := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	if !retry.Released || retry.Changed || retry.CustodyTransition == nil || !retry.CustodyTransition.Idempotent {
		t.Fatalf("idempotent retry = %#v", retry)
	}
	f.assertHeads(t, f.laterPushed, f.laterPushed, f.laterPushed)
}

// TestSupersedeStaleCustodyExactTwoRunChain pins the transition's own dogfood
// shape: the checked-out head is the old run's exact submission, the old
// preserved head is the later run's exact submission, and the later push is the
// adoption target. Both exact run edges are required; no content equivalence is
// consulted.
func TestSupersedeStaleCustodyExactTwoRunChain(t *testing.T) {
	t.Parallel()
	f := newRecoverFixture(t, types.RunFailed)
	mustRun(t, f.local, "remote", "add", "origin", f.remote)
	mustRun(t, f.local, "fetch", f.gate, f.preserved+":refs/no-mistakes/recover/"+f.run.ID)

	later, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.preserved, f.base)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := filepath.Join(t.TempDir(), "later-pipeline")
	mustRun(t, filepath.Dir(pipeline), "-c", "core.autocrlf=false", "clone", f.gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(pipeline, "later.txt"), "later exact fix\n")
	mustRun(t, pipeline, "add", "later.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(review): later exact fix")
	pushed := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")
	mustRun(t, pipeline, "push", f.remote, "HEAD:refs/heads/feature/recover")
	recordExactPushedRun(t, f.db, f.repo, later, pushed, "feature/recover", types.RunFailed)

	plan := f.service.PlanStaleCustody(f.ctx)
	if plan.Safety != "safe_stale_custody_supersession" || plan.NextAction == nil || !strings.Contains(plan.NextAction.Command, later.ID) {
		t.Fatalf("two-run plan = %#v", plan)
	}
	state := f.service.SupersedeStaleCustody(f.ctx, f.run.ID, later.ID)
	if !state.Released || !state.Changed {
		t.Fatalf("two-run transition = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != pushed {
		t.Fatalf("two-run adopted head = %s, want %s", got, pushed)
	}
	if state.CustodyTransition == nil || state.CustodyTransition.LocalHead != f.submitted || state.CustodyTransition.SubmittedHead != f.preserved || state.CustodyTransition.PushedHead != pushed {
		t.Fatalf("two-run transition identities = %#v", state.CustodyTransition)
	}
}

func TestSupersedeStaleCustodyRefusalControls(t *testing.T) {
	t.Parallel()

	t.Run("dirty worktree", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		mustWrite(t, filepath.Join(f.local, "dirty.txt"), "dirty\n")
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_dirty" {
			t.Fatalf("dirty transition = %#v", state)
		}
		f.assertHeads(t, f.laterSubmitted, f.laterPushed, f.laterPushed)
		f.assertOldCustody(t, false)
	})

	t.Run("duplicate checkout", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		duplicate := filepath.Join(t.TempDir(), "duplicate")
		mustRun(t, f.local, "worktree", "add", "--force", duplicate, f.branch)
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_branch_ambiguous" {
			t.Fatalf("duplicate transition = %#v", state)
		}
		f.assertOldCustody(t, false)
	})

	t.Run("genuine local divergence", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		mustWrite(t, filepath.Join(f.local, "unique.txt"), "unique local work\n")
		mustRun(t, f.local, "add", "unique.txt")
		mustRun(t, f.local, "commit", "-m", "unique local work")
		unique := mustRun(t, f.local, "rev-parse", "HEAD")
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_submission_mismatch" {
			t.Fatalf("diverged transition = %#v", state)
		}
		f.assertHeads(t, unique, f.laterPushed, f.laterPushed)
		f.assertOldCustody(t, false)
	})

	t.Run("missing remote branch", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		mustRun(t, f.remote, "update-ref", "-d", "refs/heads/"+f.branch)
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_remote_mismatch" {
			t.Fatalf("missing remote transition = %#v", state)
		}
		f.assertOldCustody(t, false)
	})

	t.Run("active later run", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		if err := f.db.UpdateRunStatus(f.laterRun.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_run_active" {
			t.Fatalf("active transition = %#v", state)
		}
		f.assertHeads(t, f.laterSubmitted, f.laterPushed, f.laterPushed)
		f.assertOldCustody(t, false)
	})

	t.Run("wrong later lineage stage", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.lineageRun.ID)
		if state.Released || state.Safety != "blocked_supersede_run_identity" {
			t.Fatalf("wrong later run transition = %#v", state)
		}
		f.assertHeads(t, f.laterSubmitted, f.laterPushed, f.laterPushed)
		f.assertOldCustody(t, false)
	})

	t.Run("missing exact lineage", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		if err := f.db.SetRunCustodyReturned(f.lineageRun.ID); err != nil {
			t.Fatal(err)
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_lineage_missing" {
			t.Fatalf("missing-lineage transition = %#v", state)
		}
		f.assertHeads(t, f.laterSubmitted, f.laterPushed, f.laterPushed)
		f.assertOldCustody(t, false)
	})

	t.Run("ambiguous later owner", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		duplicate, err := f.db.InsertRun(f.repo.ID, f.branch, f.laterSubmitted, f.lineagePushed)
		if err != nil {
			t.Fatal(err)
		}
		recordExactPushedRun(t, f.db, f.repo, duplicate, f.laterPushed, f.branch, types.RunFailed)
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_owner_ambiguous" {
			t.Fatalf("ambiguous-owner transition = %#v", state)
		}
		f.assertHeads(t, f.laterSubmitted, f.laterPushed, f.laterPushed)
		f.assertOldCustody(t, false)
	})

	t.Run("unrelated repository run", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		otherRoot := t.TempDir()
		otherRepo, err := f.db.InsertRepo(filepath.Join(otherRoot, "work"), filepath.Join(otherRoot, "remote.git"), "main")
		if err != nil {
			t.Fatal(err)
		}
		otherRun, err := f.db.InsertRun(otherRepo.ID, f.branch, f.laterSubmitted, f.lineagePushed)
		if err != nil {
			t.Fatal(err)
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, otherRun.ID)
		if state.Released || state.Safety != "blocked_supersede_run_identity" {
			t.Fatalf("other-repository transition = %#v", state)
		}
		f.assertHeads(t, f.laterSubmitted, f.laterPushed, f.laterPushed)
		f.assertOldCustody(t, false)
	})
}

func TestSupersedeStaleCustodyRefusesChangedRefsAndRunFacts(t *testing.T) {
	t.Parallel()

	t.Run("remote changes before local CAS", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		f.service.beforeStaleSupersessionLocalCAS = func() {
			writer := filepath.Join(t.TempDir(), "remote-writer")
			mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.remote, writer)
			configureIdentity(t, writer)
			mustRun(t, writer, "checkout", f.branch)
			mustWrite(t, filepath.Join(writer, "remote-race.txt"), "remote race\n")
			mustRun(t, writer, "add", "remote-race.txt")
			mustRun(t, writer, "commit", "-m", "remote race")
			mustRun(t, writer, "push", "origin", "HEAD:refs/heads/"+f.branch)
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_remote_changed" {
			t.Fatalf("remote race = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.laterSubmitted {
			t.Fatalf("remote race moved local head to %s", got)
		}
		f.assertOldCustody(t, false)
	})

	t.Run("gate changes before local CAS", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		f.service.beforeStaleSupersessionLocalCAS = func() {
			mustRun(t, f.gate, "update-ref", "refs/heads/"+f.branch, f.lineagePushed, f.laterPushed)
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_gate_race" {
			t.Fatalf("gate race = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.laterSubmitted {
			t.Fatalf("gate race moved local head to %s", got)
		}
		f.assertOldCustody(t, false)
	})

	t.Run("ownership generation changes before local CAS", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		f.service.beforeStaleSupersessionLocalCAS = func() {
			active, err := f.db.InsertRun(f.repo.ID, f.branch, f.laterSubmitted, f.lineagePushed)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_assumptions_changed" {
			t.Fatalf("ownership race = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.laterSubmitted {
			t.Fatalf("ownership race moved local head to %s", got)
		}
		f.assertOldCustody(t, false)
	})
}

func TestSupersedeStaleCustodyFinalBoundaryRefusesChangedFacts(t *testing.T) {
	t.Parallel()

	t.Run("remote changes after local adoption", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		f.service.beforeStaleSupersessionCommit = func() {
			writer := filepath.Join(t.TempDir(), "late-remote-writer")
			mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.remote, writer)
			configureIdentity(t, writer)
			mustRun(t, writer, "checkout", f.branch)
			mustWrite(t, filepath.Join(writer, "late-remote.txt"), "late remote\n")
			mustRun(t, writer, "add", "late-remote.txt")
			mustRun(t, writer, "commit", "-m", "late remote")
			mustRun(t, writer, "push", "origin", "HEAD:refs/heads/"+f.branch)
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_remote_changed" {
			t.Fatalf("late remote race = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.laterPushed {
			t.Fatalf("late remote race local head = %s, want safely adopted %s", got, f.laterPushed)
		}
		f.assertOldCustody(t, false)
	})

	t.Run("gate changes after local adoption", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		f.service.beforeStaleSupersessionCommit = func() {
			mustRun(t, f.gate, "update-ref", "refs/heads/"+f.branch, f.lineagePushed, f.laterPushed)
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_gate_race" {
			t.Fatalf("late gate race = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.laterPushed {
			t.Fatalf("late gate race local head = %s, want safely adopted %s", got, f.laterPushed)
		}
		f.assertOldCustody(t, false)
	})

	t.Run("new active owner appears after local adoption", func(t *testing.T) {
		f := newStaleSupersessionFixture(t)
		f.service.beforeStaleSupersessionCommit = func() {
			active, err := f.db.InsertRun(f.repo.ID, f.branch, f.laterPushed, f.lineagePushed)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
		}
		state := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
		if state.Released || state.Safety != "blocked_supersede_assumptions_changed" {
			t.Fatalf("late ownership race = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.laterPushed {
			t.Fatalf("late ownership race local head = %s, want safely adopted %s", got, f.laterPushed)
		}
		f.assertOldCustody(t, false)
	})
}

func TestSupersedeStaleCustodyCrashAfterLocalCASIsRetryable(t *testing.T) {
	t.Parallel()
	f := newStaleSupersessionFixture(t)
	f.service.afterStaleSupersessionLocalCAS = func() { panic("simulated crash after local CAS") }
	func() {
		defer func() { _ = recover() }()
		_ = f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	}()
	f.service.afterStaleSupersessionLocalCAS = nil

	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.laterPushed {
		t.Fatalf("post-crash local head = %s, want adopted %s", got, f.laterPushed)
	}
	f.assertOldCustody(t, false)

	retry := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	if !retry.Released || retry.CustodyTransition == nil {
		t.Fatalf("post-crash retry = %#v", retry)
	}
	f.assertHeads(t, f.laterPushed, f.laterPushed, f.laterPushed)
	f.assertOldCustody(t, true)
}

func TestSupersedeStaleCustodyCrashRetryRebindsUnrelatedOwnershipGeneration(t *testing.T) {
	t.Parallel()
	f := newStaleSupersessionFixture(t)
	f.service.beforeStaleSupersessionCommit = func() { panic("simulated crash before custody stamp") }
	func() {
		defer func() { _ = recover() }()
		_ = f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	}()
	f.service.beforeStaleSupersessionCommit = nil

	unrelated, err := f.db.InsertRun(f.repo.ID, f.branch, f.laterPushed, f.lineagePushed)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(unrelated.ID, types.RunCancelled, f.laterPushed); err != nil {
		t.Fatal(err)
	}

	retry := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	if !retry.Released {
		t.Fatalf("retry after unrelated ownership generation = %#v", retry)
	}
	f.assertOldCustody(t, true)
}

func TestSupersedeStaleCustodyCrashBeforeStampIsRetryable(t *testing.T) {
	t.Parallel()
	f := newStaleSupersessionFixture(t)
	f.service.beforeStaleSupersessionCommit = func() { panic("simulated crash before custody stamp") }
	func() {
		defer func() { _ = recover() }()
		_ = f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	}()
	f.service.beforeStaleSupersessionCommit = nil

	f.assertHeads(t, f.laterPushed, f.laterPushed, f.laterPushed)
	f.assertOldCustody(t, false)
	retry := f.service.SupersedeStaleCustody(f.ctx, f.oldRun.ID, f.laterRun.ID)
	if !retry.Released || retry.CustodyTransition == nil {
		t.Fatalf("pre-stamp crash retry = %#v", retry)
	}
	f.assertOldCustody(t, true)
}

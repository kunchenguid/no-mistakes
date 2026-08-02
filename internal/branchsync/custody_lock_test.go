package branchsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
)

func TestInternalMutationCapabilityRequiresActiveBranchLock(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	spec := db.InternalRefMutationSpec{
		RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch,
		Ref: "refs/heads/feature/recover", OldSHA: f.submitted, NewSHA: f.preserved,
		Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary,
	}
	if _, _, err := IssueInternalRefMutation(f.db, nil, spec); err == nil {
		t.Fatal("capability issuance without a branch lock unexpectedly succeeded")
	}
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	capability, endpoint, err := IssueInternalRefMutation(f.db, lock, spec)
	if err != nil {
		t.Fatalf("capability issuance with a branch lock: %v", err)
	}
	request := InternalRefMutationAuthorization{Capability: capability, Phase: "prepared", GatePath: spec.GatePath, Branch: spec.Branch, Ref: spec.Ref, OldSHA: spec.OldSHA, NewSHA: spec.NewSHA, Operation: spec.Operation, Scope: spec.Scope}
	if err := AuthorizeInternalRefMutation(endpoint, request); err != nil {
		t.Fatalf("live branch-lock authority rejected capability: %v", err)
	}
	lock.Release()
	if err := AuthorizeInternalRefMutation(endpoint, request); err == nil {
		t.Fatal("closed branch-lock authority accepted capability")
	}
	restarted, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Release()
	if err := AuthorizeInternalRefMutation(endpoint, request); err == nil {
		t.Fatal("restarted branch-lock authority accepted a capability from the prior owner")
	}
	if _, err := restarted.ensureInternalMutationAuthority(f.db); err != nil {
		t.Fatal(err)
	}
	if err := restarted.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IssueInternalRefMutation(f.db, restarted, spec); err == nil {
		t.Fatal("closed branch-lock file descriptor issued a capability")
	}
	restarted.file = nil
	restarted.closeInternalMutationAuthority()
}

func TestInternalMutationAuthorityClosesIdleClientsOnShutdown(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := lock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	conn, err := dialInternalMutationAuthority(endpoint)
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		lock.Release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("authority shutdown waited on an idle client")
	}
}

func TestGateRefLockBlocksHooksPathOverride(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer branchLock.Release()
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireGateRefLock(f.gate, "refs/heads/feature/recover", authority)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if _, err := gitpkg.Run(context.Background(), f.gate, "-c", "core.hooksPath="+t.TempDir(), "update-ref", "refs/heads/feature/recover", f.preserved, f.submitted); err == nil {
		t.Fatal("raw update-ref bypassed the final ordinary-ref lock")
	}
	if got, err := readLockedGateRef(f.gate, "refs/heads/feature/recover"); err != nil || got != f.preserved {
		t.Fatalf("locked gate branch = %s, want %s", got, f.preserved)
	}
}

func TestManagedGateAuthorityBlocksRawUpdateWhileIdle(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	authority, err := AcquireManagedGateRefAuthority(f.gate, "refs/heads/feature/recover")
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Release()
	if _, err := gitpkg.Run(context.Background(), f.gate, "-c", "core.hooksPath="+t.TempDir(), "update-ref", "refs/heads/feature/recover", f.submitted, f.preserved); err == nil {
		t.Fatal("raw update-ref bypassed the persistent managed gate authority")
	}
	if got, err := readLockedGateRef(f.gate, "refs/heads/feature/recover"); err != nil || got != f.preserved {
		t.Fatalf("idle gate head = %s, want %s", got, f.preserved)
	}
}

func TestManagedGateAuthorityCannotBeUnlinkedByInternalWriter(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	authority, err := AcquireManagedGateRefAuthority(f.gate, "refs/heads/feature/recover")
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Release()
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer branchLock.Release()
	ref := "refs/heads/feature/recover"
	if err := f.service.updateOrdinaryGateRef(f.ctx, branchLock, f.run.Branch, ref, f.preserved, f.submitted); err == nil {
		t.Fatal("internal writer bypassed the live managed gate authority")
	}
	if _, err := gitpkg.Run(f.ctx, f.gate, "-c", "core.hooksPath="+t.TempDir(), "update-ref", ref, f.submitted, f.preserved); err == nil {
		t.Fatal("raw writer changed the ref after internal writer refusal")
	}
	lockPath := filepath.Join(f.gate, filepath.FromSlash(ref)+".lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("managed gate authority disappeared: %v", err)
	}
}

func TestManagedGateAuthorityRejectsMarkerLoss(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	ref := "refs/heads/feature/recover"
	authority, err := AcquireManagedGateRefAuthority(f.gate, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(authority.Path()); err != nil {
		_ = authority.Release()
		t.Fatal(err)
	}
	if err := authority.Validate(f.gate, ref); err == nil {
		t.Fatal("managed gate authority accepted a missing marker")
	}
	if err := authority.Invalidate(); err != nil {
		t.Fatal(err)
	}
}

func TestStageRecoveryAnchorUsesPreparedThenCommittedAuthority(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover:refs/no-mistakes/recovery-source")
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := lock.ensureInternalMutationAuthority(f.db); err != nil {
		t.Fatal(err)
	}
	var calls int
	f.service.InternalMutationConsumed = func(string) error {
		calls++
		return nil
	}
	if safety := f.service.stageRecoveryAnchor(f.ctx, lock, f.run, f.preserved, f.anchorRef()); safety != "" {
		t.Fatalf("stageRecoveryAnchor() safety = %q", safety)
	}
	if calls != 2 {
		t.Fatalf("internal mutation phases = %d, want prepared and committed", calls)
	}
	stage, err := f.db.GetRecoveryAnchorStage(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stage == nil || stage.State != db.RecoveryAnchorStageStaged {
		t.Fatalf("recovery anchor stage = %#v, want staged", stage)
	}
}

func TestStageRecoveryAnchorTakesOverDeadPreparedAuthority(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover:refs/no-mistakes/recovery-source")
	first, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ensureInternalMutationAuthority(f.db); err != nil {
		first.Release()
		t.Fatal(err)
	}
	oldEndpoint, oldGeneration, err := first.authorityIdentity()
	if err != nil {
		first.Release()
		t.Fatal(err)
	}
	if err := f.db.PrepareRecoveryAnchorStage(db.RecoveryAnchorStage{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: f.anchorRef(), OldSHA: internalZeroObjectID(f.preserved), NewSHA: f.preserved, OwnerGeneration: oldGeneration, AuthorityEndpoint: oldEndpoint}); err != nil {
		first.Release()
		t.Fatal(err)
	}
	stage, err := f.db.GetRecoveryAnchorStage(f.run.ID)
	if err != nil {
		first.Release()
		t.Fatal(err)
	}
	if err := ensureRecoveryAnchorOwner(f.local, f.run, stage); err != nil {
		first.Release()
		t.Fatal(err)
	}
	first.Release()
	second, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if _, err := second.ensureInternalMutationAuthority(f.db); err != nil {
		t.Fatal(err)
	}
	f.service.InternalMutationConsumed = func(string) error { return nil }
	if safety := f.service.stageRecoveryAnchor(f.ctx, second, f.run, f.preserved, f.anchorRef()); safety != "" {
		t.Fatalf("takeover stage safety = %q", safety)
	}
	stage, err = f.db.GetRecoveryAnchorStage(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stage == nil || stage.State != db.RecoveryAnchorStageStaged || stage.AuthorityEndpoint == oldEndpoint {
		t.Fatalf("taken-over anchor stage = %#v", stage)
	}
}

func TestStageRecoveryAnchorHoldsPrivateRefFenceAcrossStageCommit(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover:refs/no-mistakes/recovery-source")
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := lock.ensureInternalMutationAuthority(f.db); err != nil {
		t.Fatal(err)
	}
	endpoint, generation, err := lock.authorityIdentity()
	if err != nil {
		t.Fatal(err)
	}
	stage := &db.RecoveryAnchorStage{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: f.anchorRef(), OldSHA: internalZeroObjectID(f.preserved), NewSHA: f.preserved, OwnerGeneration: generation, AuthorityEndpoint: endpoint, State: db.RecoveryAnchorStagePrepared}
	if err := f.db.PrepareRecoveryAnchorStage(*stage); err != nil {
		t.Fatal(err)
	}
	if err := ensureRecoveryAnchorOwner(f.local, f.run, stage); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.local, "update-ref", f.anchorRef(), f.preserved)
	var rawErr error
	f.service.InternalMutationConsumed = func(string) error {
		if rawErr == nil {
			_, rawErr = gitpkg.Run(f.ctx, f.local, "-c", "core.hooksPath="+t.TempDir(), "update-ref", f.anchorRef(), f.submitted, f.preserved)
		}
		return nil
	}
	if safety := f.service.stageRecoveryAnchor(f.ctx, lock, f.run, f.preserved, f.anchorRef()); safety != "" {
		t.Fatalf("private-fence stage safety = %q", safety)
	}
	if rawErr == nil {
		t.Fatal("raw private-anchor writer bypassed the authenticated private-ref fence")
	}
	if got, err := readDirectLooseWorktreeRef(f.local, f.anchorRef()); err != nil || got != f.preserved {
		t.Fatalf("private anchor after fenced stage = %q, err=%v", got, err)
	}
}

func TestRecoveryAnchorFinalizationRejectsChangedAnchor(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover:refs/no-mistakes/recovery-source")
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := lock.ensureInternalMutationAuthority(f.db); err != nil {
		t.Fatal(err)
	}
	if safety := f.service.stageRecoveryAnchor(f.ctx, lock, f.run, f.preserved, f.anchorRef()); safety != "" {
		t.Fatalf("stage recovery anchor safety = %q", safety)
	}
	mustRun(t, f.local, "update-ref", f.anchorRef(), f.submitted)
	if err := f.service.withVerifiedRecoveryAnchor(f.ctx, f.run, func() error { return nil }); err == nil {
		t.Fatal("changed recovery anchor passed final custody verification")
	}
}

func TestRecoveryAnchorOwnerUpgradeIsCanonical(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	stage := &db.RecoveryAnchorStage{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: f.anchorRef(), OldSHA: internalZeroObjectID(f.preserved), NewSHA: f.preserved, OwnerGeneration: "generation-1", AuthorityEndpoint: "endpoint-1", State: db.RecoveryAnchorStagePrepared}
	path, err := recoveryAnchorOwnerPath(f.local, stage.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := recoveryAnchorOwnerValue(f.run, stage) + "generation=old\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureRecoveryAnchorOwner(f.local, f.run, stage); err != nil {
		t.Fatalf("upgrade recovery anchor owner: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != recoveryAnchorOwnerValue(f.run, stage) {
		t.Fatalf("canonical owner = %q", got)
	}
}

func TestManagedPrivateRefAuthorityWritesExactAnchor(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover:refs/no-mistakes/recovery-source")
	gitDir, err := worktreeGitDir(f.local)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := AcquireManagedPrivateRefAuthority(gitDir, f.anchorRef())
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Release()
	if err := authority.UpdateRef(f.ctx, gitDir, f.anchorRef(), internalZeroObjectID(f.preserved), f.preserved); err != nil {
		t.Fatalf("private ref authority update: %v", err)
	}
}

func TestReconcileOrdinaryGateRefHoldsLockAcrossJournalUpdate(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	authority, err := lock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, generation, err := lock.authorityIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/feature/recover"
	gateLock, err := acquireGateRefLock(f.gate, ref, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	owner := gateRefLockOwner{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, OwnerGeneration: generation, AuthorityEndpoint: endpoint, ExpectedHead: f.preserved}
	gateLock.owner = owner
	if err := gateLock.setOwner(owner); err != nil {
		t.Fatal(err)
	}
	gateLock.database = f.db
	if err := f.db.PrepareGateRefLock(db.GateRefLockJournal{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, LockPath: gateLock.path, OwnerGeneration: generation, AuthorityEndpoint: endpoint, ExpectedHead: f.preserved, NewHead: f.submitted, FileIdentity: gateLock.identity}); err != nil {
		t.Fatal(err)
	}
	spec := db.InternalRefMutationSpec{RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, OldSHA: f.preserved, NewSHA: f.submitted, Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary}
	capability, _, err := IssueInternalRefMutation(f.db, lock, spec)
	if err != nil {
		t.Fatal(err)
	}
	request := InternalRefMutationAuthorization{Capability: capability, Phase: "prepared", GatePath: f.gate, Branch: f.run.Branch, Ref: ref, OldSHA: f.preserved, NewSHA: f.submitted, Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary}
	if err := AuthorizeInternalRefMutation(authority, request); err != nil {
		t.Fatal(err)
	}
	mutationCtx := gitpkg.WithSanitizedGateConfig(context.Background())
	if err := gateLock.commitRef(mutationCtx, f.gate, ref, f.preserved, f.submitted); err != nil {
		t.Fatal(err)
	}
	request.Phase = "committed"
	if err := AuthorizeInternalRefMutation(authority, request); err != nil {
		t.Fatal(err)
	}
	gateLock.closeKeepJournal()
	rawErr := error(nil)
	f.service.beforeGateRefReconcile = func() {
		_, rawErr = gitpkg.Run(context.Background(), f.gate, "-c", "core.hooksPath="+t.TempDir(), "update-ref", ref, f.preserved, f.submitted)
	}
	if err := f.service.updateOrdinaryGateRef(context.Background(), lock, f.run.Branch, ref, f.preserved, f.submitted); err != nil {
		t.Fatal(err)
	}
	if rawErr == nil {
		t.Fatal("raw writer changed the ref during reconciliation")
	}
	if got := mustRun(t, f.gate, "rev-parse", ref); got != f.submitted {
		t.Fatalf("reconciled gate head = %s, want %s", got, f.submitted)
	}
}

func TestReadLockedGateRefRejectsLinkedRefs(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	refPath := filepath.Join(f.gate, filepath.FromSlash("refs/heads/feature/recover"))
	if err := os.WriteFile(refPath, []byte("ref: refs/heads/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readLockedGateRef(f.gate, "refs/heads/feature/recover"); err == nil {
		t.Fatal("symbolic ordinary ref was accepted")
	}

	payload := filepath.Join(t.TempDir(), "linked-ref")
	if err := os.WriteFile(payload, []byte(f.preserved+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(refPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(payload, refPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readLockedGateRef(f.gate, "refs/heads/feature/recover"); err == nil {
		t.Fatal("symlinked ordinary ref was accepted")
	}
}

func TestUpdateGateRefRefusesBeforeOrdinaryMutation(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer branchLock.Release()
	f.service.InternalMutationConsumed = func(string) error { return errors.New("live authority refused mutation") }
	if err := f.service.updateGateRef(f.ctx, branchLock, f.run.Branch, "refs/heads/feature/recover", f.preserved, f.submitted); err == nil {
		t.Fatal("ordinary mutation unexpectedly succeeded")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("ordinary gate ref changed to %s, want %s", got, f.preserved)
	}
}

func TestGateRefLockRemovesStaleOwnedLockAfterAuthorityExit(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock, err := acquireGateRefLock(f.gate, "refs/heads/feature/recover", authority)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	if err := gateLock.file.Close(); err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock.file = nil
	branchLock.closeInternalMutationAuthority()
	if err := branchLock.file.Close(); err != nil {
		t.Fatal(err)
	}
	branchLock.file = nil

	restarted, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Release()
	newAuthority, err := restarted.ensureInternalMutationAuthority(f.db)
	if err != nil {
		t.Fatal(err)
	}
	newGateLock, err := acquireGateRefLock(f.gate, "refs/heads/feature/recover", newAuthority)
	if err != nil {
		t.Fatalf("stale owned gate lock blocked retry: %v", err)
	}
	newGateLock.Release()
}

func TestStampedRecoveryReclaimsOwnedGateLockAfterCrash(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	generation, err := newGateRefLockGeneration()
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	ref := "refs/heads/" + f.run.Branch
	owner := gateRefLockOwner{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved}
	gateLock, err := acquireOwnedGateRefLock(f.gate, ref, owner)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	if err := f.db.PrepareGateRefLock(db.GateRefLockJournal{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, LockPath: gateLock.path, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved, FileIdentity: gateLock.identity}); err != nil {
		gateLock.Release()
		branchLock.Release()
		t.Fatal(err)
	}
	if err := f.db.SetRunCustodyReturnedCAS(f.run); err != nil {
		gateLock.Release()
		branchLock.Release()
		t.Fatal(err)
	}
	if err := gateLock.file.Close(); err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock.file = nil
	branchLock.Release()

	state := f.service.Recover(f.ctx, true)
	if !state.Recovered {
		t.Fatalf("stamped recovery did not reclaim crashed gate lock: %#v", state)
	}
	if _, err := os.Stat(gateLock.path); !os.IsNotExist(err) {
		t.Fatalf("crashed gate lock still exists: %v", err)
	}
	if journal, err := f.db.GetGateRefLock(f.run.ID); err != nil || journal != nil {
		t.Fatalf("stale gate lock journal = %#v, %v", journal, err)
	}
}

func TestGateRefLockReleaseRetainsJournalWhenRemovalFails(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	generation, err := newGateRefLockGeneration()
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	ref := "refs/heads/" + f.run.Branch
	owner := gateRefLockOwner{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved}
	gateLock, err := acquireOwnedGateRefLock(f.gate, ref, owner)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	if err := f.db.PrepareGateRefLock(db.GateRefLockJournal{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, LockPath: gateLock.path, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved, FileIdentity: gateLock.identity}); err != nil {
		gateLock.Release()
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock.database = f.db
	originalRemove := removeGateRefLock
	removeGateRefLock = func(string) error { return errors.New("injected removal failure") }
	err = gateLock.Release()
	removeGateRefLock = originalRemove
	if err == nil {
		t.Fatal("gate lock release unexpectedly ignored removal failure")
	}
	if journal, journalErr := f.db.GetGateRefLock(f.run.ID); journalErr != nil || journal == nil {
		t.Fatalf("gate lock journal after failed removal = %#v, %v", journal, journalErr)
	}
	_ = originalRemove(gateLock.path)
	_ = f.db.ClearGateRefLock(f.run.ID, generation)
	branchLock.Release()
}

func TestCustodyLockRejectsLiveSecondAttemptAndReleasesAfterOwnerExit(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	first, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireCustodyLock(f.service, f.run)
	if second != nil || !errors.Is(err, ErrCustodyLockHeld) {
		t.Fatalf("second custody lock = %#v, %v", second, err)
	}
	first.Release()
	third, err := acquireCustodyLock(f.service, f.run)
	if err != nil || third == nil {
		t.Fatalf("custody lock after owner release = %#v, %v", third, err)
	}
	third.Release()
}

func TestCustodyLockIsSharedByRepositoryBranchAcrossRuns(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	first, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	newer := *f.run
	newer.ID = "newer-run"
	second, err := acquireCustodyLock(f.service, &newer)
	if second != nil || !errors.Is(err, ErrCustodyLockHeld) {
		t.Fatalf("newer run custody lock = %#v, %v", second, err)
	}

	otherBranch := newer
	otherBranch.ID = "other-branch-run"
	otherBranch.Branch = "other-branch"
	third, err := acquireCustodyLock(f.service, &otherBranch)
	if err != nil || third == nil {
		t.Fatalf("other branch custody lock = %#v, %v", third, err)
	}
	third.Release()
}

func TestCustodyLockFailurePreservesNonContentionErrors(t *testing.T) {
	state := State{State: StatePipelineOwned}
	if got := custodyLockFailure(state, fmt.Errorf("permission denied")); got.Safety != "blocked_recover_custody_lock" {
		t.Fatalf("non-contention lock failure = %#v", got)
	}
	if got := custodyLockFailure(state, fmt.Errorf("%w: busy", ErrCustodyLockHeld)); got.Safety != "blocked_recover_custody_race" {
		t.Fatalf("contention lock failure = %#v", got)
	}
}

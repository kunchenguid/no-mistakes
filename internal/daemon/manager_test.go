package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// --- RunManager integration tests ---

func TestValidateRecoveredSessionProviders_RejectsUnavailableFixerProvider(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(pipeline.SessionRoleFixer), "codex", "fixer-session"); err != nil {
		t.Fatal(err)
	}
	claude, err := agent.New(types.AgentClaude, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer claude.Close()
	if err := validateRecoveredSessionProviders(database, run.ID, claude); err == nil || !strings.Contains(err.Error(), `session provider "codex" is no longer configured`) {
		t.Fatalf("validate recovered fixer provider error = %v", err)
	}
}

func TestPushReceivedTracksRunTelemetry(t *testing.T) {
	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "telemetry-run-repo")
	commitTestReceive(t, d, "telemetry-run-repo", p.RepoDir("telemetry-run-repo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("telemetry-run-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}

	started := recorder.find("run", "action", "started")
	if started == nil {
		t.Fatal("expected run started telemetry event")
	}
	if got := started.fields["trigger"]; got != "push" {
		t.Fatalf("started trigger = %v, want push", got)
	}
	if got := started.fields["agent"]; got != string(types.AgentClaude) {
		t.Fatalf("started agent = %v, want %q", got, types.AgentClaude)
	}
	if got := started.fields["branch_role"]; got != "default" {
		t.Fatalf("started branch_role = %v, want default", got)
	}

	// The executor persists terminal status before its owner goroutine emits
	// terminal telemetry. Wait for that asynchronous handoff instead of
	// assuming it completed in the same scheduling slice, which is especially
	// unreliable on Windows.
	finished := waitForTelemetryEvent(t, recorder, "run", "action", "finished")
	if finished == nil {
		t.Fatal("expected run finished telemetry event")
	}
	if got := finished.fields["status"]; got != string(types.RunCompleted) {
		t.Fatalf("finished status = %v, want %q", got, types.RunCompleted)
	}
	if _, ok := finished.fields["duration_ms"]; !ok {
		t.Fatal("expected duration_ms in run finished telemetry")
	}
}

func TestLegacyPublicationProofRejectsUnsupportedTarget(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	a := strings.Repeat("a", 40)
	run := &db.Run{HeadSHA: a, SubmittedHeadSHA: &a}
	manager := &RunManager{db: database, paths: p}
	if err := manager.legacyPublicationProof(context.Background(), run, "feature", []string{"https://unsupported.example/repo.git"}); err == nil {
		t.Fatal("legacy publication proof accepted an unsupported target")
	}
}

func TestValidatePublicationLedgerRequiresHistoricalProof(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "remote", "add", "origin", "https://unsupported.example/repo.git")
	repo, err := database.InsertRepoWithID("historical-proof", workDir, "https://unsupported.example/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)
	if err := manager.validatePublicationLedger(context.Background(), run); err == nil {
		t.Fatal("publication ledger passed without historical provider proof")
	}
}

func TestVerifyRemotePublicationProofRejectsPreservedHeads(t *testing.T) {
	t.Run("ordinary branch", func(t *testing.T) {
		p, database := newRefreshRunFixture(t)
		bare := filepath.Join(t.TempDir(), "remote.git")
		work := filepath.Join(t.TempDir(), "work")
		gitCmd(t, "", "init", "--bare", bare)
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, work, "init")
		gitCmd(t, work, "config", "user.email", "test@test.com")
		gitCmd(t, work, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("A\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, work, "add", "file.txt")
		gitCmd(t, work, "commit", "-m", "A")
		a := gitOutput(t, work, "rev-parse", "HEAD")
		gitCmd(t, work, "branch", "-M", "feature")
		gitCmd(t, work, "push", bare, "feature:refs/heads/feature")
		if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("P\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, work, "commit", "-am", "P")
		pHead := gitOutput(t, work, "rev-parse", "HEAD")
		gitCmd(t, bare, "fetch", work, pHead+":refs/keep/recovery")
		gitCmd(t, bare, "update-ref", "refs/heads/feature", pHead, a)
		repo, err := database.InsertRepoWithID("remote-proof-branch", work, bare, "main")
		if err != nil {
			t.Fatal(err)
		}
		run, err := database.InsertRun(repo.ID, "feature", pHead, a)
		if err != nil {
			t.Fatal(err)
		}
		run.SubmittedHeadSHA = &a
		recorded, err := database.ListRunPublicationTargets(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		manager := NewRunManager(database, p, nil)
		t.Cleanup(manager.Shutdown)
		if err := manager.verifyRemotePublicationProof(context.Background(), run, repo, []publicationTargetURL{{kind: "upstream", url: bare}}, recorded); err == nil {
			t.Fatal("remote branch containing preserved head passed publication proof")
		}
	})

	t.Run("pull request ref", func(t *testing.T) {
		p, database := newRefreshRunFixture(t)
		bare := filepath.Join(t.TempDir(), "remote.git")
		work := filepath.Join(t.TempDir(), "work")
		gitCmd(t, "", "init", "--bare", bare)
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, work, "init")
		gitCmd(t, work, "config", "user.email", "test@test.com")
		gitCmd(t, work, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("A\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, work, "add", "file.txt")
		gitCmd(t, work, "commit", "-m", "A")
		a := gitOutput(t, work, "rev-parse", "HEAD")
		gitCmd(t, work, "branch", "-M", "feature")
		if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("P\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, work, "commit", "-am", "P")
		pHead := gitOutput(t, work, "rev-parse", "HEAD")
		gitCmd(t, bare, "fetch", work, pHead+":refs/keep/recovery")
		gitCmd(t, bare, "update-ref", "refs/heads/feature", a)
		gitCmd(t, bare, "update-ref", "refs/pull/7/head", pHead)
		repo, err := database.InsertRepoWithID("remote-proof-pr", work, bare, "main")
		if err != nil {
			t.Fatal(err)
		}
		run, err := database.InsertRun(repo.ID, "feature", pHead, a)
		if err != nil {
			t.Fatal(err)
		}
		run.SubmittedHeadSHA = &a
		recorded, err := database.ListRunPublicationTargets(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		manager := NewRunManager(database, p, nil)
		t.Cleanup(manager.Shutdown)
		if _, err := manager.verifyRemotePublicationSnapshotWithRequestRefs(context.Background(), run, repo, []publicationTargetURL{{kind: "upstream", url: bare}}, recorded, map[string][]string{
			recorded[0].TargetFingerprint: []string{"refs/pull/7/head"},
		}); err == nil {
			t.Fatal("remote pull-request ref containing preserved head passed publication proof")
		}
	})
}

func TestRecoveryTargetIdentityRejectsMismatchedTarget(t *testing.T) {
	prURL := "https://github.com/example/parent/pull/7"
	if _, err := recoveryTargetIdentity(context.Background(), "https://github.com/example/fork.git", &prURL); err == nil {
		t.Fatal("mismatched publication target was accepted")
	}
	identity, err := recoveryTargetIdentity(context.Background(), "https://github.com/example/parent.git", &prURL)
	if err != nil || identity != prURL {
		t.Fatalf("matching publication identity = %q, %v", identity, err)
	}
}

func TestRecoveryTargetIdentitySeparatesPublicationTargets(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	parent := "https://github.com/example/parent.git"
	fork := "https://github.com/example/fork.git"
	repo, err := database.InsertRepoWithFork(t.TempDir(), parent, fork, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/example/parent/pull/7"
	if err := database.UpdateRunPRURLForTarget(run.ID, prURL, "upstream", db.PublicationTargetFingerprint(parent)); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)
	targets := []string{parent, fork}

	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.recoveryTargetIdentityForRun(context.Background(), parent, targets, run); err == nil {
		t.Fatal("published PR target passed unpublished recovery validation")
	}
}

func TestManagedGateMissingLedgerIsQuarantined(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "managed-missing-ledger")
	ref := "refs/heads/main"
	manager := NewRunManager(database, p, nil)
	if err := manager.ensureManagedGateGuard(repo, ref); err == nil {
		t.Fatal("managed gate authority was accepted without a ledger row")
	}
	quarantine, err := database.GetGateRefQuarantine(repo.ID, p.RepoDir(repo.ID), ref)
	if err != nil {
		t.Fatal(err)
	}
	if quarantine == nil || quarantine.ObservedHead != head {
		t.Fatalf("missing-ledger quarantine = %#v, want observed head %s", quarantine, head)
	}
	manager.Shutdown()
}

func TestRestoreManagedGateGuardsQuarantinesUnjournaledBranch(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "restore-missing-ledger")
	ref := "refs/heads/main"
	manager := NewRunManager(database, p, nil)
	manager.restoreManagedGateGuards()
	quarantine, err := database.GetGateRefQuarantine(repo.ID, p.RepoDir(repo.ID), ref)
	if err != nil {
		t.Fatal(err)
	}
	if quarantine == nil || quarantine.ObservedHead != head {
		t.Fatalf("startup quarantine = %#v, want observed head %s", quarantine, head)
	}
	manager.Shutdown()
}

func TestRestoreManagedGateGuardsPropagatesEnumerationError(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "restore-enumeration-error")
	if err := os.RemoveAll(p.RepoDir(repo.ID)); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	defer manager.Shutdown()
	if err := manager.restoreManagedGateGuards(); err == nil {
		t.Fatal("managed gate enumeration error was swallowed")
	}
}

func TestRestoreManagedGateGuardsPropagatesQuarantinePersistenceError(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	setupTestGitRepo(t, p, database, "restore-quarantine-error")
	manager := NewRunManager(database, p, nil)
	defer manager.Shutdown()
	manager.quarantineGateRef = func(string, string, string, string, string, string) error {
		return errors.New("quarantine persistence failed")
	}
	if err := manager.restoreManagedGateGuards(); err == nil {
		t.Fatal("quarantine persistence error was swallowed")
	}
}

func TestManagedGateAuthorityLossPersistsQuarantine(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "managed-authority-loss")
	ref := "refs/heads/main"
	if err := database.SetManagedGateRefHead(repo.ID, p.RepoDir(repo.ID), ref, head); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	if err := manager.ensureManagedGateGuard(repo, ref); err != nil {
		t.Fatal(err)
	}
	guard := manager.managedGateGuards[managedGateGuardKey(repo.ID, ref)]
	if guard == nil {
		t.Fatal("managed gate guard was not installed")
	}
	if err := os.Remove(guard.Path()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ensureManagedGateGuard(repo, ref); err == nil {
		t.Fatal("managed gate authority loss was accepted")
	}
	quarantine, err := database.GetGateRefQuarantine(repo.ID, p.RepoDir(repo.ID), ref)
	if err != nil {
		t.Fatal(err)
	}
	if quarantine == nil {
		t.Fatal("managed gate authority loss was not quarantined")
	}
	if err := manager.ensureManagedGateGuard(repo, ref); err == nil {
		t.Fatal("quarantined managed gate authority was reacquired")
	}
	manager.Shutdown()
}

func TestManagedGateFinalizeRollsBackAfterAuthorityLoss(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "managed-finalize-rollback")
	ref := "refs/heads/main"
	if err := database.SetManagedGateRefHead(repo.ID, p.RepoDir(repo.ID), ref, head); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	if err := manager.ensureManagedGateGuard(repo, ref); err != nil {
		t.Fatal(err)
	}
	guard := manager.managedGateGuards[managedGateGuardKey(repo.ID, ref)]
	if guard == nil {
		t.Fatal("managed gate guard was not installed")
	}
	rolledBack := false
	err := manager.managedGateRefFinalize(repo.ID, p.RepoDir(repo.ID), ref)(context.Background(), ref, head, func() error {
		if err := os.Remove(guard.Path()); err != nil {
			return err
		}
		return nil
	}, func() error {
		rolledBack = true
		return nil
	})
	if err == nil || !rolledBack {
		t.Fatalf("finalize authority-loss result = %v, rolledBack=%v", err, rolledBack)
	}
	manager.Shutdown()
}

func TestManagedGateQuarantinePersistenceFailureStaysSticky(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "managed-quarantine-persist-failure")
	ref := "refs/heads/main"
	if err := database.SetManagedGateRefHead(repo.ID, p.RepoDir(repo.ID), ref, head); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	manager.quarantineGateRef = func(string, string, string, string, string, string) error {
		return errors.New("quarantine storage unavailable")
	}
	if err := manager.ensureManagedGateGuard(repo, ref); err != nil {
		t.Fatal(err)
	}
	guard := manager.managedGateGuards[managedGateGuardKey(repo.ID, ref)]
	if err := os.Remove(guard.Path()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ensureManagedGateGuard(repo, ref); err == nil {
		t.Fatal("authority loss with failed quarantine persistence was accepted")
	}
	if err := manager.ensureManagedGateGuard(repo, ref); err == nil {
		t.Fatal("authority was reacquired after failed quarantine persistence")
	}
	manager.Shutdown()
}

func TestRecoveryReconcilesQuarantineBeforeManagedGuard(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "recovery-quarantine-reconcile")
	ref := "refs/heads/main"
	if err := database.SetManagedGateRefHead(repo.ID, p.RepoDir(repo.ID), ref, head); err != nil {
		t.Fatal(err)
	}
	if err := database.QuarantineGateRef(repo.ID, p.RepoDir(repo.ID), ref, head, head, "authority-lost"); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	if err := manager.reconcileManagedGateQuarantine(context.Background(), repo, ref); err != nil {
		t.Fatalf("reconcile managed gate quarantine: %v", err)
	}
	if err := manager.ensureManagedGateGuard(repo, ref); err != nil {
		t.Fatalf("ensure managed gate guard after reconciliation: %v", err)
	}
	quarantine, err := database.GetGateRefQuarantine(repo.ID, p.RepoDir(repo.ID), ref)
	if err != nil {
		t.Fatal(err)
	}
	if quarantine != nil {
		t.Fatalf("quarantine remains after authenticated reconciliation: %#v", quarantine)
	}
	manager.Shutdown()
}

func TestRecoveryReconcilesQuarantineAfterStaleManagedGuard(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "recovery-stale-guard")
	ref := "refs/heads/main"
	if err := database.SetManagedGateRefHead(repo.ID, p.RepoDir(repo.ID), ref, head); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	if err := manager.ensureManagedGateGuard(repo, ref); err != nil {
		t.Fatal(err)
	}
	oldGuard := manager.managedGateGuards[managedGateGuardKey(repo.ID, ref)]
	if oldGuard == nil {
		t.Fatal("managed gate guard was not installed")
	}
	if err := database.QuarantineGateRef(repo.ID, p.RepoDir(repo.ID), ref, head, head, "authority-lost"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldGuard.Path()); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileManagedGateQuarantine(context.Background(), repo, ref); err != nil {
		t.Fatalf("reconcile stale managed gate guard: %v", err)
	}
	newGuard := manager.managedGateGuards[managedGateGuardKey(repo.ID, ref)]
	if newGuard == nil || newGuard == oldGuard {
		t.Fatal("stale managed gate guard was retained")
	}
	manager.Shutdown()
}

func TestManagedGateLedgerQuarantinesRawProjectionWrite(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "managed-ledger-raw-write")
	ref := "refs/heads/main"
	if err := database.SetManagedGateRefHead(repo.ID, p.RepoDir(repo.ID), ref, head); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	if err := manager.ensureManagedGateGuard(repo, ref); err != nil {
		t.Fatal(err)
	}
	guard := manager.managedGateGuards[managedGateGuardKey(repo.ID, ref)]
	if guard == nil {
		t.Fatal("managed gate guard was not installed")
	}
	if err := os.Remove(guard.Path()); err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	if _, err := git.Run(git.WithSanitizedGateConfig(context.Background()), p.RepoDir(repo.ID), "-c", "core.hooksPath="+t.TempDir(), "update-ref", ref, zero, head); err != nil {
		t.Fatalf("raw projection write: %v", err)
	}
	if err := manager.ensureManagedGateGuard(repo, ref); err == nil {
		t.Fatal("raw projection write was accepted by managed ledger")
	}
	quarantine, err := database.GetGateRefQuarantine(repo.ID, p.RepoDir(repo.ID), ref)
	if err != nil {
		t.Fatal(err)
	}
	if quarantine == nil {
		t.Fatal("raw projection write was not quarantined")
	}
	manager.Shutdown()
}

func TestPushReceivedSkipStepsConfiguresExecutor(t *testing.T) {
	review := &mockPassStep{name: types.StepReview}
	testStep := &mockPassStep{name: types.StepTest}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review, testStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "skip-run-repo")
	commitTestReceiveWithOptions(t, d, "skip-run-repo", p.RepoDir("skip-run-repo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA, []types.StepName{types.StepReview}, "")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("skip-run-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		SkipSteps:         []types.StepName{types.StepReview},
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}
	if got := review.execCnt.Load(); got != 0 {
		t.Fatalf("review executed %d times, want 0", got)
	}
	if got := testStep.execCnt.Load(); got != 1 {
		t.Fatalf("test executed %d times, want 1", got)
	}
	steps, err := d.GetStepsByRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview && step.Status != types.StepStatusSkipped {
			t.Fatalf("review status = %s, want %s", step.Status, types.StepStatusSkipped)
		}
	}
}

func TestPushReservationSurvivesOwnershipLockContention(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, oldSHA := setupTestGitRepo(t, p, d, "reserved-push-repo")
	if _, err := d.ReplaceRepoURLs(repo.ID, p.RepoDir(repo.ID), ""); err != nil {
		t.Fatalf("use local publication target: %v", err)
	}
	gitCmd(t, repo.WorkingPath, "config", "user.email", "test@test.com")
	gitCmd(t, repo.WorkingPath, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "received.txt"), []byte("received\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "received.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "received")
	newSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/tmp/reserved-push")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	params := &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: oldSHA, New: newSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability}
	var admitted ipc.AdmitPushResult
	if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: params.Gate, Ref: params.Ref, Old: params.Old, New: params.New, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability}, &admitted); err != nil {
		t.Fatalf("admit push: %v", err)
	}
	params.ReceiveCapability = testReceiveCapability
	transactionCapability := testReceiveCapability
	for _, phase := range []string{"prepared"} {
		if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: params.Gate, Phase: phase, ReservationID: admitted.ReservationID, Ref: params.Ref, Old: params.Old, New: params.New, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: transactionCapability}, nil); err != nil {
			t.Fatalf("receive transaction %s: %v", phase, err)
		}
	}
	gitCmd(t, p.RepoDir(repo.ID), "update-ref", "refs/heads/main", newSHA, oldSHA)
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: params.Gate, Phase: "committed", ReservationID: admitted.ReservationID, Ref: params.Ref, Old: params.Old, New: params.New, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: transactionCapability}, nil); err != nil {
		t.Fatalf("receive transaction committed: %v", err)
	}

	owner, err := branchsync.AcquireBranchOwnershipLock(p, repo, repo.WorkingPath, "main")
	if err != nil {
		t.Fatal(err)
	}
	var blocked ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, params, &blocked); err == nil {
		t.Fatal("push notification succeeded while ownership lock was held")
	}
	pending, err := d.GetPendingReceiveReservation(repo.ID, "main", params.Ref, oldSHA, newSHA)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.State != db.ReceiveReservationCommitted {
		t.Fatalf("reservation after blocked notification = %#v, want committed", pending)
	}
	owner.Release()

	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, params, &result); err != nil {
		t.Fatalf("retry push notification: %v", err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}
	reservation, err := d.GetReceiveReservation(pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.State != db.ReceiveReservationPublished || reservation.RunID == nil || *reservation.RunID != result.RunID {
		t.Fatalf("published reservation = %#v", reservation)
	}
}

func TestPendingReceiveDoesNotReuseRunBeforeRefMutation(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, oldSHA := setupTestGitRepo(t, p, d, "pending-receive-old-ref-repo")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "next.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "next")
	newSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	if _, err := d.InsertRun(repo.ID, "main", newSHA, oldSHA); err != nil {
		t.Fatal(err)
	}
	reservation, err := d.ReserveReceiveForSession(repo.ID, p.RepoDir(repo.ID), "main", "refs/heads/main", oldSHA, newSHA, testReceiveSessionID, testReceiveCapability, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: oldSHA, New: newSHA,
		ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability,
	}, &result)
	if err == nil {
		t.Fatal("notification succeeded before the gate ref reached the reserved head")
	}
	got, err := d.GetReceiveReservation(reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != db.ReceiveReservationReserved {
		t.Fatalf("reservation state = %q, want reserved", got.State)
	}
	if runs, err := d.GetRunsByRepoHead(repo.ID, "main", newSHA); err != nil {
		t.Fatal(err)
	} else if len(runs) != 1 {
		t.Fatalf("runs for unreceived head = %d, want historical run only", len(runs))
	}
}

func TestPushReceivedRefusesUnboundNotification(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "unbound-notification-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("unbound-notification-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &result)
	if err == nil || !strings.Contains(err.Error(), "evidence is missing") {
		t.Fatalf("unbound notification error = %v, want missing evidence", err)
	}
	if runs, err := d.GetRunsByRepo("unbound-notification-repo"); err != nil {
		t.Fatal(err)
	} else if len(runs) != 0 {
		t.Fatalf("runs after unbound notification = %d, want 0", len(runs))
	}
}

func TestAdmitPushRejectsCallerIssuedReceiveCredentials(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "caller-issued-receive-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.AdmitPushResult
	err = client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{
		Gate:              p.RepoDir("caller-issued-receive-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  "caller-chosen-session",
		ReceiveCapability: "caller-chosen-capability",
	}, &result)
	if err == nil || !strings.Contains(err.Error(), "was not issued") {
		t.Fatalf("caller-issued receive credentials error = %v", err)
	}
	if reservations, err := d.GetPendingReceiveReservations(); err != nil {
		t.Fatal(err)
	} else if len(reservations) != 0 {
		t.Fatalf("caller-issued credentials created %d reservations", len(reservations))
	}
}

func TestPushReceivedRejectsReservationIDWithWrongCapability(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "forged-reservation-notification-repo")
	commitTestReceive(t, d, "forged-reservation-notification-repo", p.RepoDir("forged-reservation-notification-repo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)
	reservation, err := d.GetLatestReceiveReservation("forged-reservation-notification-repo", "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if reservation == nil {
		t.Fatal("receive reservation is missing")
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir("forged-reservation-notification-repo"), Ref: "refs/heads/main", Old: "0000000000000000000000000000000000000000", New: headSHA, ReservationID: reservation.ID, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: "forged-capability"}, &result)
	if err == nil || (!strings.Contains(err.Error(), "session does not match") && !strings.Contains(err.Error(), "session is no longer active")) {
		t.Fatalf("forged reservation notification error = %v", err)
	}
	if runs, err := d.GetRunsByRepo("forged-reservation-notification-repo"); err != nil {
		t.Fatal(err)
	} else if len(runs) != 0 {
		t.Fatalf("runs after forged notification = %d, want 0", len(runs))
	}
}

func TestReceiveCreatesDistinctRunForSameHeadWithDifferentReservation(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, oldSHA := setupTestGitRepo(t, p, d, "same-head-receive-repo")
	gitCmd(t, repo.WorkingPath, "config", "user.email", "test@test.com")
	gitCmd(t, repo.WorkingPath, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "preserved.txt"), []byte("preserved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "preserved.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "preserved")
	preservedSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/tmp/preserved")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	gatePath := p.RepoDir(repo.ID)
	ref := "refs/heads/main"
	var admitted ipc.AdmitPushResult
	if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: gatePath, Ref: ref, Old: oldSHA, New: preservedSHA, Intent: "first", ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability}, &admitted); err != nil {
		t.Fatal(err)
	}
	firstParams := &ipc.PushReceivedParams{Gate: gatePath, Ref: ref, Old: oldSHA, New: preservedSHA, Intent: "first", ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability}
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "prepared", ReservationID: admitted.ReservationID, Ref: ref, Old: oldSHA, New: preservedSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability}, nil); err != nil {
		t.Fatalf("first receive transaction prepared: %v", err)
	}
	gitCmd(t, gatePath, "update-ref", ref, preservedSHA, oldSHA)
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "committed", ReservationID: admitted.ReservationID, Ref: ref, Old: oldSHA, New: preservedSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability}, nil); err != nil {
		t.Fatalf("first receive transaction committed: %v", err)
	}
	var firstResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, firstParams, &firstResult); err != nil {
		t.Fatal(err)
	}
	firstRun := waitForRunTerminalState(t, d, firstResult.RunID)

	gitCmd(t, repo.WorkingPath, "checkout", "-b", "alternate", oldSHA)
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "alternate.txt"), []byte("alternate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "alternate.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "alternate")
	alternateSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/tmp/alternate")
	setupSession := "test-receive-session-setup"
	if err := d.RegisterReceiveSession(repo.ID, gatePath, setupSession, testReceiveCapability); err != nil {
		t.Fatal(err)
	}
	var setupAdmitted ipc.AdmitPushResult
	if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: gatePath, Ref: ref, Old: preservedSHA, New: alternateSHA, Intent: "setup", ReceiveSessionID: setupSession, ReceiveCapability: testReceiveCapability}, &setupAdmitted); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "prepared", ReservationID: setupAdmitted.ReservationID, Ref: ref, Old: preservedSHA, New: alternateSHA, ReceiveSessionID: setupSession, ReceiveCapability: testReceiveCapability}, nil); err != nil {
		t.Fatalf("setup receive transaction prepared: %v", err)
	}
	gitCmd(t, gatePath, "update-ref", ref, alternateSHA, preservedSHA)
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "committed", ReservationID: setupAdmitted.ReservationID, Ref: ref, Old: preservedSHA, New: alternateSHA, ReceiveSessionID: setupSession, ReceiveCapability: testReceiveCapability}, nil); err != nil {
		t.Fatalf("setup receive transaction committed: %v", err)
	}
	var setupResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: gatePath, Ref: ref, Old: preservedSHA, New: alternateSHA, Intent: "setup", ReceiveSessionID: setupSession, ReceiveCapability: testReceiveCapability}, &setupResult); err != nil {
		t.Fatalf("setup push received: %v", err)
	}
	waitForRunTerminalState(t, d, setupResult.RunID)

	secondSession := "test-receive-session-second"
	if err := d.RegisterReceiveSession(repo.ID, gatePath, secondSession, testReceiveCapability); err != nil {
		t.Fatal(err)
	}
	secondParams := &ipc.AdmitPushParams{Gate: gatePath, Ref: ref, Old: alternateSHA, New: preservedSHA, Intent: "second", ReceiveSessionID: secondSession, ReceiveCapability: testReceiveCapability}
	if err := client.Call(ipc.MethodAdmitPush, secondParams, &admitted); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "prepared", ReservationID: admitted.ReservationID, Ref: ref, Old: alternateSHA, New: preservedSHA, ReceiveSessionID: secondSession, ReceiveCapability: testReceiveCapability}, nil); err != nil {
		t.Fatalf("second receive transaction prepared: %v", err)
	}
	gitCmd(t, gatePath, "update-ref", ref, preservedSHA, alternateSHA)
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "committed", ReservationID: admitted.ReservationID, Ref: ref, Old: alternateSHA, New: preservedSHA, ReceiveSessionID: secondSession, ReceiveCapability: testReceiveCapability}, nil); err != nil {
		t.Fatalf("second receive transaction committed: %v", err)
	}
	var secondResult ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: gatePath, Ref: ref, Old: alternateSHA, New: preservedSHA, Intent: "second", ReceiveSessionID: secondSession, ReceiveCapability: testReceiveCapability}, &secondResult); err != nil {
		t.Fatal(err)
	}
	if secondResult.RunID == firstResult.RunID {
		t.Fatal("same-head receive reused the prior reservation's run")
	}
	secondRun := waitForRunTerminalState(t, d, secondResult.RunID)
	if secondRun.Intent == nil || *secondRun.Intent != "second" {
		t.Fatalf("second receive intent = %v, want second", secondRun.Intent)
	}
	if firstRun.Intent == nil || *firstRun.Intent != "first" {
		t.Fatalf("first receive intent = %v, want first", firstRun.Intent)
	}
}

func TestReceiveDeletionReservationReconcilesWithoutRun(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, oldSHA := setupTestGitRepo(t, p, d, "deletion-receive-repo")
	gatePath := p.RepoDir(repo.ID)
	ref := "refs/heads/main"
	zeroSHA := "0000000000000000000000000000000000000000"

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var admitted ipc.AdmitPushResult
	if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: gatePath, Ref: ref, Old: oldSHA, New: zeroSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: testReceiveCapability}, &admitted); err != nil {
		t.Fatal(err)
	}
	deletionCapability := testReceiveCapability
	if pending, err := d.GetPendingReceiveReservationsForBranch(repo.ID, "main"); err != nil || len(pending) != 1 {
		t.Fatalf("pending deletion reservations = %d, %v", len(pending), err)
	}
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "prepared", ReservationID: admitted.ReservationID, Ref: ref, Old: oldSHA, New: zeroSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: deletionCapability}, nil); err != nil {
		t.Fatalf("deletion receive transaction prepared: %v", err)
	}
	gitCmd(t, gatePath, "update-ref", "-d", ref, oldSHA)
	if err := client.Call(ipc.MethodReceiveTransaction, &ipc.ReceiveTransactionParams{Gate: gatePath, Phase: "committed", ReservationID: admitted.ReservationID, Ref: ref, Old: oldSHA, New: zeroSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: deletionCapability}, nil); err != nil {
		t.Fatalf("deletion receive transaction committed: %v", err)
	}

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: gatePath, Ref: ref, Old: oldSHA, New: zeroSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: deletionCapability}, &result)
	if err != nil || !result.Deleted {
		t.Fatalf("deletion notification result = %+v, error = %v; want typed success", result, err)
	}
	if runs, err := d.GetRunsByRepo(repo.ID); err != nil {
		t.Fatal(err)
	} else if len(runs) != 0 {
		t.Fatalf("runs after deletion = %d, want 0", len(runs))
	}
	reservations, err := d.GetPendingReceiveReservationsForBranch(repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(reservations) != 0 {
		t.Fatalf("pending reservations after deletion = %d, want 0", len(reservations))
	}

	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: gatePath, Ref: ref, Old: oldSHA, New: zeroSHA, ReceiveSessionID: testReceiveSessionID, ReceiveCapability: deletionCapability}, &result); err != nil || !result.Deleted {
		t.Fatalf("duplicate deletion result = %+v, error = %v; want typed success", result, err)
	}
}

func TestPushReceivedAllowsDifferentBranchRunsConcurrently(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "concurrent-branch-repo")
	commitTestReceive(t, d, "concurrent-branch-repo", p.RepoDir("concurrent-branch-repo"), "feature/one", "refs/heads/feature/one", "0000000000000000000000000000000000000000", headSHA)
	commitTestReceive(t, d, "concurrent-branch-repo", p.RepoDir("concurrent-branch-repo"), "feature/two", "refs/heads/feature/two", "0000000000000000000000000000000000000000", headSHA)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("concurrent-branch-repo"),
		Ref:               "refs/heads/feature/one",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &first); err != nil {
		t.Fatal(err)
	}
	waitForStartedBranch(t, started, "feature/one")

	var second ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("concurrent-branch-repo"),
		Ref:               "refs/heads/feature/two",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &second); err != nil {
		t.Fatal(err)
	}
	waitForStartedBranch(t, started, "feature/two")

	for _, tc := range []struct {
		branch string
		runID  string
	}{
		{branch: "feature/one", runID: first.RunID},
		{branch: "feature/two", runID: second.RunID},
	} {
		active, err := d.GetActiveRun("concurrent-branch-repo", tc.branch)
		if err != nil {
			t.Fatalf("get active run for %s: %v", tc.branch, err)
		}
		if active == nil {
			t.Fatalf("expected active run for %s", tc.branch)
		}
		if active.ID != tc.runID {
			t.Fatalf("active run for %s = %s, want %s", tc.branch, active.ID, tc.runID)
		}
		if active.Status != types.RunRunning {
			t.Fatalf("active run for %s status = %s, want running", tc.branch, active.Status)
		}
	}
}

type notifyBlockStep struct {
	name    types.StepName
	started chan<- string
}

func (s *notifyBlockStep) Name() types.StepName { return s.name }

func (s *notifyBlockStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	select {
	case s.started <- sctx.Run.Branch:
	default:
	}
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

func waitForStartedBranch(t *testing.T, started <-chan string, branch string) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case got := <-started:
			if got == branch {
				return
			}
		case <-timeout:
			t.Fatalf("run for branch %s did not start", branch)
		}
	}
}

// TestPushReceivedConcurrentDifferentBranchRunsAvoidSharedConfigLock fires two
// branch pushes for the same repo at the same time so both runs hit worktree
// creation and git-identity setup concurrently. All runs share one gate bare
// repo, so writing identity with `git config --local` (which targets the bare's
// shared config) made the two startups race on <bare>/config.lock and fail one
// run with "could not lock config file ...: File exists". CopyLocalUserIdentity
// now writes per-worktree, so the startups no longer contend. The race window
// is during synchronous startRun, so a failure surfaces directly as the
// push_received call's error. macOS-only in practice (Linux file locking and
// timing hide it), but the assertion is platform-independent.
func TestPushReceivedConcurrentDifferentBranchRunsAvoidSharedConfigLock(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})

	const repoID = "concurrent-config-lock-repo"
	_, headSHA := setupTestGitRepo(t, p, d, repoID)
	commitTestReceive(t, d, repoID, p.RepoDir(repoID), "feature/one", "refs/heads/feature/one", "0000000000000000000000000000000000000000", headSHA)
	commitTestReceive(t, d, repoID, p.RepoDir(repoID), "feature/two", "refs/heads/feature/two", "0000000000000000000000000000000000000000", headSHA)

	// Mirror a real gate: enable the per-worktree config isolation that
	// `no-mistakes init` installs, which is what lets identity writes avoid the
	// shared config.lock.
	if err := git.IsolateHooksPath(context.Background(), p.RepoDir(repoID)); err != nil {
		t.Fatalf("isolate hooks path: %v", err)
	}

	branches := []string{"feature/one", "feature/two"}
	errs := make([]error, len(branches))
	var wg sync.WaitGroup
	for i, br := range branches {
		wg.Add(1)
		go func(i int, br string) {
			defer wg.Done()
			// A dedicated client per goroutine: a single client serializes
			// calls, which would defeat the concurrency we are testing.
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				errs[i] = err
				return
			}
			defer client.Close()
			var res ipc.PushReceivedResult
			errs[i] = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate:              p.RepoDir(repoID),
				Ref:               "refs/heads/" + br,
				Old:               "0000000000000000000000000000000000000000",
				New:               headSHA,
				ReceiveSessionID:  testReceiveSessionID,
				ReceiveCapability: testReceiveCapability,
			}, &res)
		}(i, br)
	}
	wg.Wait()

	for i, br := range branches {
		if errs[i] != nil {
			t.Fatalf("concurrent push for %s failed: %v", br, errs[i])
		}
	}

	// Drain both start signals regardless of which run won the race to begin,
	// then confirm both branches have a live, error-free run.
	gotStarted := make(map[string]bool, len(branches))
	for range branches {
		select {
		case b := <-started:
			gotStarted[b] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("a concurrent run did not start (started so far: %v)", gotStarted)
		}
	}

	for _, br := range branches {
		if !gotStarted[br] {
			t.Fatalf("run for branch %s did not start", br)
		}
		active, err := d.GetActiveRun(repoID, br)
		if err != nil {
			t.Fatalf("get active run for %s: %v", br, err)
		}
		if active == nil {
			t.Fatalf("expected active run for %s", br)
		}
		if active.Status != types.RunRunning {
			t.Fatalf("active run for %s status = %s, want running (error: %v)", br, active.Status, active.Error)
		}
	}
}

func TestRerunSkipStepsConfiguresExecutor(t *testing.T) {
	review := &mockPassStep{name: types.StepReview}
	testStep := &mockPassStep{name: types.StepTest}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review, testStep}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "skip-rerun-repo")
	commitTestReceive(t, d, "skip-rerun-repo", p.RepoDir("skip-rerun-repo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("skip-rerun-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, first.RunID)

	var second ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID:    "skip-rerun-repo",
		Branch:    "main",
		SkipSteps: []types.StepName{types.StepReview},
	}, &second)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, second.RunID)

	if got := review.execCnt.Load(); got != 1 {
		t.Fatalf("review executed %d times, want 1", got)
	}
	if got := testStep.execCnt.Load(); got != 2 {
		t.Fatalf("test executed %d times, want 2", got)
	}
	steps, err := d.GetStepsByRun(second.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview && step.Status != types.StepStatusSkipped {
			t.Fatalf("review status = %s, want %s", step.Status, types.StepStatusSkipped)
		}
	}
}

func TestRerunInheritsIntentFromSelectedRun(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "selected-rerun-repo")
	commitTestReceive(t, d, "selected-rerun-repo", p.RepoDir("selected-rerun-repo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("selected-rerun-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, first.RunID)
	selectedIntent := "  selected exact requirements\n"
	if err := d.UpdateRunIntent(first.RunID, db.RunIntent{Summary: selectedIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}

	newer, err := d.InsertRunWithIntent("selected-rerun-repo", "main", headSHA, headSHA, &db.RunIntent{
		Summary: "newer unrelated requirements",
		Source:  db.RunIntentSourceAgent,
		Score:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(newer.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	var rerun ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID:        "selected-rerun-repo",
		Branch:        "main",
		PreviousRunID: first.RunID,
	}, &rerun)
	if err != nil {
		t.Fatal(err)
	}
	got := waitForRunTerminalState(t, d, rerun.RunID)
	if got.Intent == nil || *got.Intent != selectedIntent {
		t.Fatalf("intent = %v, want %q", got.Intent, selectedIntent)
	}
	if got.IntentSource == nil || *got.IntentSource != db.RunIntentSourceRerun {
		t.Fatalf("intent source = %v, want %q", got.IntentSource, db.RunIntentSourceRerun)
	}
}

func TestPushReceivedReturnsBeforeIntentSummarization(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	slowClaude := writeSlowMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+slowClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, headSHA := setupTestGitRepo(t, p, d, "intent-start-run-repo")
	commitTestReceive(t, d, "intent-start-run-repo", p.RepoDir("intent-start-run-repo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)
	writeManagerClaudeFixture(t, fakeHome, repo.WorkingPath, []string{
		`{"type":"user","cwd":` + testJSONString(t, repo.WorkingPath) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please update test.txt"}}`,
	})

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	started := time.Now()
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("intent-start-run-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	// The 3s slowClaude script is not on this test's synchronous path (the
	// review step here is a mockPassStep and the "claude" agent is explicit,
	// so ResolveAgent never probes it): what this bound really guards is
	// startRun's synchronous git plumbing (worktree add, identity copy,
	// fetch, resolve-ref, config loads) staying well clear of the 3s the
	// pipeline goroutine's slow agent call would take if it ever ran inline.
	// Windows CI process-spawn overhead across those several git subprocess
	// calls is much higher than on macOS/Linux, so Windows gets generous
	// headroom while non-Windows keeps the tight bound that would catch a
	// real regression in startRun's synchronous git plumbing.
	maxElapsed := 2500 * time.Millisecond
	if runtimeGOOS == "windows" {
		maxElapsed = 8 * time.Second
	}
	if elapsed := time.Since(started); elapsed > maxElapsed {
		t.Fatalf("PushReceived took %s, want under %s", elapsed, maxElapsed)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty run ID")
	}

	waitForRunTerminalState(t, d, result.RunID)
}

func writeManagerClaudeFixture(t *testing.T, home, repoCWD string, lines []string) {
	t.Helper()
	encoded := testClaudeProjectDirName(repoCWD)
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-uuid-1.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPushReceivedTracksRunTelemetryAfterPanic(t *testing.T) {
	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	step := &mockPanicStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, headSHA := setupTestGitRepo(t, p, d, "telemetry-panic-repo")
	commitTestReceive(t, d, "telemetry-panic-repo", p.RepoDir("telemetry-panic-repo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("telemetry-panic-repo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Error != nil && strings.Contains(*run.Error, "internal panic") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finished := recorder.find("run", "action", "finished")
	if finished == nil {
		t.Fatal("expected run finished telemetry event after panic")
	}
	if got := finished.fields["status"]; got != string(types.RunFailed) {
		t.Fatalf("finished status = %v, want %q", got, types.RunFailed)
	}
	if _, ok := finished.fields["duration_ms"]; !ok {
		t.Fatal("expected duration_ms in run finished telemetry after panic")
	}
	for _, field := range []string{"agent_invocations", "resumed_invocations", "fallback_invocations"} {
		if got, ok := finished.fields[field]; !ok || got != 0 {
			t.Fatalf("%s = %v, want 0", field, got)
		}
	}
}

func TestPushReceivedDemoModeBypassesAgentResolution(t *testing.T) {
	t.Setenv("NM_DEMO", "1")

	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: /path/that/does/not/exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, headSHA := setupTestGitRepo(t, p, d, "testrepo-demo")
	commitTestReceive(t, d, "testrepo-demo", p.RepoDir("testrepo-demo"), "main", "refs/heads/main", "0000000000000000000000000000000000000000", headSHA)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:              p.RepoDir("testrepo-demo"),
		Ref:               "refs/heads/main",
		Old:               "0000000000000000000000000000000000000000",
		New:               headSHA,
		ReceiveSessionID:  testReceiveSessionID,
		ReceiveCapability: testReceiveCapability,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" {
		t.Fatal("expected non-empty run ID")
	}

	waitForRunTerminalState(t, d, result.RunID)
	run, err := d.GetRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		t.Fatalf("run status = %q, want %q (error: %s)", run.Status, types.RunCompleted, runErr)
	}
	if step.execCnt.Load() == 0 {
		t.Error("mock step was never executed")
	}
}

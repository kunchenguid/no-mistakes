package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/ownerdecision"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

func protectedRunConfig(t *testing.T, repoID, branch, initialHeadSHA string) (*ipc.OwnerDecisionRunConfig, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ownerdecision.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	genesisHead, err := ownerdecision.GenesisHeadForRun(publicKey, repoID, branch, initialHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	return &ipc.OwnerDecisionRunConfig{PublicKey: encoded, ExpectedHead: genesisHead}, privateKey
}

func waitForProtectedStepStatus(t *testing.T, database interface {
	GetStepsByRun(string) ([]*db.StepResult, error)
}, runID string, stepName types.StepName, status types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err == nil {
			for _, step := range steps {
				if step.StepName == stepName && step.Status == status {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("step %s did not reach %s", stepName, status)
}

func waitForProtectedRunTerminal(t *testing.T, database *db.DB, runID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status.Terminal() {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach terminal state", runID)
	return nil
}

func TestProtectedRunRequiresSignedGateDecisionAndPublishesVerifiableHead(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	})
	repo, headSHA := setupTestGitRepo(t, p, database, "protected-gate-repo")
	config, privateKey := protectedRunConfig(t, repo.ID, "main", headSHA)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	wrong := *config
	wrong.ExpectedHead = ownerdecision.DigestBytes([]byte("not-genesis"))
	var rejected ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("protected-gate-repo"), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: headSHA, OwnerDecision: &wrong,
	}, &rejected); err == nil {
		t.Fatal("fresh protected run accepted a non-genesis expected head")
	}

	var started ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("protected-gate-repo"), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: headSHA, OwnerDecision: config,
	}, &started); err != nil {
		t.Fatal(err)
	}
	waitForProtectedStepStatus(t, database, started.RunID, types.StepReview, types.StepStatusAwaitingApproval)

	var superseding ipc.PushReceivedResult
	supersedeErr := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("protected-gate-repo"), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: headSHA,
	}, &superseding)
	if supersedeErr == nil || !strings.Contains(supersedeErr.Error(), "signed cancellation") {
		t.Fatalf("unsigned protected-run supersede error = %v", supersedeErr)
	}
	stillRunning, err := database.GetRun(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.Status != types.RunRunning || stillRunning.AwaitingAgentSince == nil {
		t.Fatalf("unsigned supersede changed protected run: status=%s awaiting=%v", stillRunning.Status, stillRunning.AwaitingAgentSince)
	}

	var info ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: started.RunID}, &info); err != nil {
		t.Fatal(err)
	}
	if info.Run == nil || !info.Run.OwnerDecisionProtected || info.Run.OwnerDecisionHead != config.ExpectedHead {
		t.Fatalf("protected run info = %+v", info.Run)
	}
	var response ipc.RespondResult
	legacyErr := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: started.RunID, Step: types.StepReview, Action: types.ActionApprove}, &response)
	if legacyErr == nil || !strings.Contains(legacyErr.Error(), "signed decision envelope") {
		t.Fatalf("legacy protected response error = %v", legacyErr)
	}

	var challenge ipc.OwnerDecisionChallengeResult
	if err := client.Call(ipc.MethodOwnerDecisionChallenge, &ipc.OwnerDecisionChallengeParams{RunID: started.RunID, Purpose: ownerdecision.PurposeRespond}, &challenge); err != nil {
		t.Fatal(err)
	}
	envelope, err := ownerdecision.Sign(privateKey, challenge.Challenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: started.RunID, Decision: &envelope}, &response); err != nil {
		t.Fatal(err)
	}
	completed := waitForRunTerminalState(t, database, started.RunID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("protected run status = %s", completed.Status)
	}
	head, protected, err := database.OwnerDecisionHead(started.RunID)
	if err != nil || !protected || head == config.ExpectedHead {
		t.Fatalf("protected history head = %q protected=%v err=%v", head, protected, err)
	}
	if err := database.VerifyOwnerDecisionHistory(started.RunID, head); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedRunRequiresSignedCancellation(t *testing.T) {
	startedStep := make(chan struct{})
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepTest, started: startedStep}}
	})
	repo, headSHA := setupTestGitRepo(t, p, database, "protected-cancel-repo")
	config, privateKey := protectedRunConfig(t, repo.ID, "main", headSHA)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var started ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("protected-cancel-repo"), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: headSHA, OwnerDecision: config,
	}, &started); err != nil {
		t.Fatal(err)
	}
	select {
	case <-startedStep:
	case <-time.After(5 * time.Second):
		t.Fatal("protected run did not start")
	}
	var cancelResult ipc.CancelRunResult
	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: started.RunID}, &cancelResult); err == nil || !strings.Contains(err.Error(), "signed cancellation envelope") {
		t.Fatalf("unsigned cancellation error = %v", err)
	}
	var challenge ipc.OwnerDecisionChallengeResult
	if err := client.Call(ipc.MethodOwnerDecisionChallenge, &ipc.OwnerDecisionChallengeParams{RunID: started.RunID, Purpose: ownerdecision.PurposeCancel}, &challenge); err != nil {
		t.Fatal(err)
	}
	envelope, err := ownerdecision.Sign(privateKey, challenge.Challenge, ownerdecision.Response{Action: types.ActionAbort})
	if err != nil {
		t.Fatal(err)
	}
	crossHead := envelope.Clone()
	crossHead.Challenge.PreviousHead = ownerdecision.DigestBytes([]byte("cross-head"))
	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: started.RunID, Decision: &crossHead}, &cancelResult); err == nil {
		t.Fatal("cross-head cancellation was accepted")
	}
	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: started.RunID, Decision: &envelope}, &cancelResult); err != nil {
		t.Fatal(err)
	}
	terminal := waitForProtectedRunTerminal(t, database, started.RunID)
	if terminal.Status != types.RunCancelled && terminal.Status != types.RunFailed {
		t.Fatalf("cancelled protected run status = %s", terminal.Status)
	}
}

func TestProtectedRestartStaysParkedUntilSignedExternalHeadCheckpoint(t *testing.T) {
	root, err := os.MkdirTemp("", "dtest-protected-restart")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, "no-mistakes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, headSHA := setupTestGitRepo(t, p, database, "protected-restart-repo")
	run, err := database.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ProtectRunOwnerDecisions(run.ID, publicKey); err != nil {
		t.Fatal(err)
	}
	authority, err := database.GetOwnerDecisionAuthority(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	genesisHead := authority.GenesisHead
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"high","description":"owner choice","action":"ask-user"}]}`
	if err := database.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, headSHA, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, database, func() []pipeline.Step {
			return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
		})
	}()
	defer func() {
		client, dialErr := ipc.Dial(p.Socket())
		if dialErr == nil {
			_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			_ = client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(35 * time.Second):
			t.Error("protected restart daemon did not stop")
		}
	}()
	waitForDaemonReady(t, p)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var response ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: run.ID, Step: types.StepReview, Action: types.ActionApprove}, &response); err == nil {
		t.Fatal("protected recovered gate resumed before signed checkpoint")
	}
	stillParked, err := database.GetRun(run.ID)
	if err != nil || stillParked.Status != types.RunRunning || stillParked.AwaitingAgentSince == nil {
		t.Fatalf("protected restart state = %+v, %v", stillParked, err)
	}

	var checkpointChallenge ipc.OwnerDecisionChallengeResult
	if err := client.Call(ipc.MethodOwnerDecisionChallenge, &ipc.OwnerDecisionChallengeParams{
		RunID: run.ID, Purpose: ownerdecision.PurposeCheckpoint, ExpectedHead: genesisHead,
	}, &checkpointChallenge); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := ownerdecision.Sign(privateKey, checkpointChallenge.Challenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	wrong := checkpointChallenge.Challenge
	wrong.Nonce += "-replayed-or-tampered"
	wrongCheckpoint, err := ownerdecision.Sign(privateKey, wrong, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	var checkpointResult ipc.OwnerDecisionCheckpointResult
	if err := client.Call(ipc.MethodOwnerDecisionCheckpoint, &ipc.OwnerDecisionCheckpointParams{RunID: run.ID, Decision: wrongCheckpoint}, &checkpointResult); err == nil {
		t.Fatal("wrong external history head resumed protected run")
	}
	if err := client.Call(ipc.MethodOwnerDecisionCheckpoint, &ipc.OwnerDecisionCheckpointParams{RunID: run.ID, Decision: checkpoint}, &checkpointResult); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var gateChallenge ipc.OwnerDecisionChallengeResult
	for {
		err = client.Call(ipc.MethodOwnerDecisionChallenge, &ipc.OwnerDecisionChallengeParams{RunID: run.ID, Purpose: ownerdecision.PurposeRespond}, &gateChallenge)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("protected recovered gate did not arm: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	approval, err := ownerdecision.Sign(privateKey, gateChallenge.Challenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: run.ID, Decision: &approval}, &response); err != nil {
		t.Fatal(err)
	}
	completed := waitForProtectedRunTerminal(t, database, run.ID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("protected recovered run status = %s", completed.Status)
	}
}

func TestProtectedRestartCheckpointNonceRefusesRollbackReplay(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "checkpoint-replay.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := database.ProtectRunOwnerDecisions(run.ID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"owner choice","action":"ask-user"}]}`
	if err := database.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, run.HeadSHA, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	plan := recoveredRunPlan{run: run, repo: repo}
	firstManager := NewRunManager(database, paths.WithRoot(t.TempDir()), func() []pipeline.Step { return nil })
	firstManager.pendingProtected[run.ID] = plan
	checkpointH1, err := firstManager.HandleOwnerDecisionChallenge(run.ID, ownerdecision.PurposeCheckpoint, authority.GenesisHead)
	if err != nil {
		t.Fatal(err)
	}
	oldCheckpoint, err := ownerdecision.Sign(privateKey, checkpointH1, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	decisionChallenge := ownerdecision.Challenge{
		Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeRespond,
		RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: authority.InitialHeadSHA, GateHeadSHA: run.HeadSHA,
		Step: types.StepReview, StepResultID: step.ID, RoundID: round.ID,
		FindingsDigest: ownerdecision.DigestBytes([]byte(findings)), PreviousHead: authority.GenesisHead,
		Nonce:    "respond:" + round.ID + ":" + authority.GenesisHead,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	decision, err := ownerdecision.Sign(privateKey, decisionChallenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := database.AppendOwnerDecision(run.ID, ownerdecision.PurposeRespond+":"+round.ID, decision, decisionChallenge, &db.OwnerDecisionProjection{
		RoundID: round.ID, SelectedFindingIDs: db.DeclinedSelectionJSON, SelectionSource: db.RoundSelectionSourceUserDeclined,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM owner_decision_events WHERE run_id = ?`, run.ID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE step_rounds SET selected_finding_ids = NULL, selection_source = NULL, user_findings_json = NULL WHERE id = ?`, round.ID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	secondManager := NewRunManager(database, paths.WithRoot(t.TempDir()), func() []pipeline.Step { return nil })
	secondManager.pendingProtected[run.ID] = plan
	if err := secondManager.HandleOwnerDecisionCheckpoint(run.ID, oldCheckpoint); err == nil || !strings.Contains(err.Error(), "fresh daemon-issued challenge") {
		t.Fatalf("old checkpoint replay after restart error = %v", err)
	}
	if _, err := secondManager.HandleOwnerDecisionChallenge(run.ID, ownerdecision.PurposeCheckpoint, appended.Head); err == nil {
		t.Fatal("fresh checkpoint challenge accepted controller H2 against rolled-back local H1")
	}
	freshH1, err := secondManager.HandleOwnerDecisionChallenge(run.ID, ownerdecision.PurposeCheckpoint, authority.GenesisHead)
	if err != nil {
		t.Fatal(err)
	}
	if freshH1.Nonce == checkpointH1.Nonce {
		t.Fatal("daemon restart reused the prior checkpoint nonce")
	}
}

func TestStartupFailsIncompleteProtectedPendingBeforeProviderAndAllowsFreshRun(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, headSHA := setupTestGitRepo(t, p, database, "protected-pending-crash-repo")
	run, err := database.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ProtectRunOwnerDecisions(run.ID, publicKey); err != nil {
		t.Fatal(err)
	}
	cancelRun, err := database.InsertRun(repo.ID, "cancel-feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	cancelAuthority, err := database.ProtectRunOwnerDecisions(cancelRun.ID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(cancelRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	cancelWorktree := p.WorktreeDir(repo.ID, cancelRun.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), cancelWorktree, headSHA); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cancelWorktree, "cancelled-work.txt"), []byte("preserve me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitpkg.Run(context.Background(), cancelWorktree, "add", "cancelled-work.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitpkg.Run(context.Background(), cancelWorktree, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "unpublished protected work"); err != nil {
		t.Fatal(err)
	}
	advancedHead, err := gitpkg.HeadSHA(context.Background(), cancelWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(cancelRun.ID, advancedHead); err != nil {
		t.Fatal(err)
	}
	cancelRun.HeadSHA = advancedHead
	now := time.Now().UTC()
	cancelChallenge := ownerdecision.Challenge{
		Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeCancel,
		RunID: cancelRun.ID, RepoID: repo.ID, Branch: cancelRun.Branch,
		HeadSHA: cancelAuthority.InitialHeadSHA, GateHeadSHA: cancelRun.HeadSHA,
		PreviousHead: cancelAuthority.GenesisHead, Nonce: "cancel:" + cancelRun.ID + ":" + cancelAuthority.GenesisHead,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	cancelEnvelope, err := ownerdecision.Sign(privateKey, cancelChallenge, ownerdecision.Response{Action: types.ActionAbort})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendOwnerDecision(cancelRun.ID, ownerdecision.PurposeCancel+":"+cancelChallenge.Nonce, cancelEnvelope, cancelChallenge, nil, now); err != nil {
		t.Fatal(err)
	}
	providerSetupCalls := 0
	mgr := NewRunManager(database, p, func() []pipeline.Step {
		providerSetupCalls++
		return nil
	})
	recoverOnStartup(database, p, mgr, worktrees.New(p, nil))
	after, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunFailed || after.HeadSHA != headSHA || providerSetupCalls != 0 {
		t.Fatalf("incomplete protected startup recovery = %+v provider_setup_calls=%d", after, providerSetupCalls)
	}
	cancelAfter, err := database.GetRun(cancelRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelAfter.Status != types.RunCancelled {
		t.Fatalf("committed pre-crash cancellation recovered as %s", cancelAfter.Status)
	}
	if cancelAfter.TerminalHeadVerifiedAt == nil {
		t.Fatal("committed cancellation was terminalized without anchored head evidence")
	}
	anchoredHead, err := gitpkg.Run(context.Background(), p.RepoDir(repo.ID), "rev-parse", "refs/no-mistakes/recover/"+cancelRun.ID)
	if err != nil || anchoredHead != advancedHead {
		t.Fatalf("committed cancellation lost unpublished head: anchor=%s want=%s err=%v", anchoredHead, advancedHead, err)
	}
	if err := mgr.cancelActiveRuns(repo.ID, run.Branch); err != nil {
		t.Fatalf("failed protected row still wedged branch: %v", err)
	}
	if _, err := database.InsertRun(repo.ID, run.Branch, "fresh-head", "base"); err != nil {
		t.Fatalf("fresh run could not proceed after recovery: %v", err)
	}
}

func TestProtectedRestartRefusesRunIdentityRewritesBeforeProviderSetup(t *testing.T) {
	mutations := map[string]func(*testing.T, string, *db.DB, *db.Run){
		"repo": func(t *testing.T, dbPath string, database *db.DB, run *db.Run) {
			other, err := database.InsertRepo(t.TempDir(), "https://example.invalid/other.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			rewriteRunForOwnerDecisionTest(t, dbPath, run.ID, "repo_id", other.ID)
		},
		"branch": func(t *testing.T, dbPath string, _ *db.DB, run *db.Run) {
			rewriteRunForOwnerDecisionTest(t, dbPath, run.ID, "branch", "rewritten")
		},
		"head": func(t *testing.T, dbPath string, _ *db.DB, run *db.Run) {
			rewriteRunForOwnerDecisionTest(t, dbPath, run.ID, "head_sha", "rewritten-head")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			dbPath := p.DB()
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/protected.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", "sealed-head", "base")
			if err != nil {
				t.Fatal(err)
			}
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := database.ProtectRunOwnerDecisions(run.ID, publicKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"review-1","severity":"high","description":"owner choice","action":"ask-user"}]}`
			if err := database.SetStepFindings(step.ID, findings); err != nil {
				t.Fatal(err)
			}
			round, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, run.HeadSHA, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			challenge := ownerdecision.Challenge{
				Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeRespond,
				RunID: run.ID, RepoID: repo.ID, Branch: run.Branch,
				HeadSHA: authority.InitialHeadSHA, GateHeadSHA: run.HeadSHA,
				Step: types.StepReview, StepResultID: step.ID, RoundID: round.ID,
				FindingsDigest: ownerdecision.DigestBytes([]byte(findings)), PreviousHead: authority.GenesisHead,
				Nonce:    "respond:" + round.ID + ":" + authority.GenesisHead,
				IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
			}
			envelope, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.AppendOwnerDecision(run.ID, ownerdecision.PurposeRespond+":"+round.ID, envelope, challenge, &db.OwnerDecisionProjection{
				RoundID: round.ID, SelectedFindingIDs: db.DeclinedSelectionJSON, SelectionSource: db.RoundSelectionSourceUserDeclined,
			}, now); err != nil {
				t.Fatal(err)
			}
			mutate(t, dbPath, database, run)
			rewritten, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			providerSetupCalls := 0
			mgr := NewRunManager(database, p, func() []pipeline.Step {
				providerSetupCalls++
				return nil
			})
			if _, err := mgr.prepareRecoveredRun(context.Background(), rewritten); err == nil {
				t.Fatal("rewritten protected run prepared for restart")
			}
			if providerSetupCalls != 0 {
				t.Fatalf("provider setup ran %d times before identity refusal", providerSetupCalls)
			}
		})
	}
}

func rewriteRunForOwnerDecisionTest(t *testing.T, dbPath, runID, column, value string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE runs SET `+column+` = ? WHERE id = ?`, value, runID); err != nil {
		t.Fatal(err)
	}
}

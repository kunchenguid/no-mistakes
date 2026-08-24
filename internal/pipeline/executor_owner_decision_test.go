package pipeline

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ownerdecision"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func protectExecutorRun(t *testing.T, database *db.DB, exec *Executor, runID string) ed25519.PrivateKey {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ProtectRunOwnerDecisions(runID, publicKey); err != nil {
		t.Fatal(err)
	}
	authority, err := database.GetOwnerDecisionAuthority(runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.ArmOwnerDecisionHistory(runID, authority.GenesisHead); err != nil {
		t.Fatal(err)
	}
	return privateKey
}

func TestExecutorSignedResponseOnUnprotectedExecutorFailsWithoutPanic(t *testing.T) {
	exec := &Executor{}
	if err := exec.RespondAuthorized(ownerdecision.Envelope{}); err == nil {
		t.Fatal("unprotected executor accepted a signed response")
	}
}

func TestExecutorProtectedGateAppendsBeforeResumeAndRefusesUnsignedTamperedExpired(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	findings := `{"findings":[{"id":"review-1","severity":"high","description":"owner choice","action":"ask-user"}],"summary":"one"}`
	exec := NewExecutor(database, paths, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	privateKey := protectExecutorRun(t, database, exec, run.ID)

	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "signed decision envelope") {
		t.Fatalf("unsigned response error = %v", err)
	}

	challenge, err := exec.OwnerDecisionChallenge(ownerdecision.PurposeRespond)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}

	tampered := valid.Clone()
	tampered.Challenge.RunID = "cross-run"
	if err := exec.RespondAuthorized(tampered); err == nil {
		t.Fatal("cross-run response was accepted")
	}
	expiredChallenge := challenge
	expiredChallenge.IssuedAt = time.Now().Add(-2 * time.Minute).Unix()
	expiredChallenge.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	expired, err := ownerdecision.Sign(privateKey, expiredChallenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.RespondAuthorized(expired); err == nil {
		t.Fatal("expired response was accepted")
	}
	if head, protected, err := database.OwnerDecisionHead(run.ID); err != nil || !protected || head != challenge.PreviousHead {
		t.Fatalf("head after refusals = %q protected=%v err=%v", head, protected, err)
	}

	appendObserved := false
	exec.beforeOwnerResume = func() {
		head, protected, err := database.OwnerDecisionHead(run.ID)
		if err != nil || !protected || head == challenge.PreviousHead {
			t.Errorf("journal was not committed before resume: head=%q protected=%v err=%v", head, protected, err)
			return
		}
		rounds, err := database.GetRoundsByStep(challenge.StepResultID)
		if err != nil || len(rounds) != 1 || rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceUserDeclined {
			t.Errorf("projection was not committed before resume: rounds=%+v err=%v", rounds, err)
			return
		}
		appendObserved = true
	}
	if err := exec.RespondAuthorized(valid); err != nil {
		t.Fatalf("valid response: %v", err)
	}
	waitExecutorDone(t, done)
	if !appendObserved {
		t.Fatal("append-before-resume seam was not observed")
	}
}

func TestExecutorProtectedHistoryTamperStopsLaterStepBeforeEffect(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	findings := `{"findings":[{"id":"review-1","severity":"high","description":"owner choice","action":"ask-user"}]}`
	effectCalls := 0
	exec := NewExecutor(database, paths, nil, nil, []Step{
		newApprovalStep(types.StepReview, findings),
		&adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
			effectCalls++
			return &StepOutcome{}, nil
		}},
	}, nil)
	privateKey := protectExecutorRun(t, database, exec, run.ID)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	challenge, err := exec.OwnerDecisionChallenge(ownerdecision.PurposeRespond)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	exec.beforeOwnerResume = func() {
		changed := `["attacker-selected"]`
		if err := database.SetStepRoundUserDecision(challenge.RoundID, &changed, db.RoundSelectionSourceUser, nil); err != nil {
			t.Errorf("inject projection tamper: %v", err)
		}
	}
	if err := exec.RespondAuthorized(envelope); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "owner decision history verification failed") {
			t.Fatalf("executor error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor did not fail closed after tamper")
	}
	if effectCalls != 0 {
		t.Fatalf("later external-effect step executed %d times", effectCalls)
	}
}

func TestExecutorProtectedHistoryTamperStopsSignedFixReexecution(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	findings := `{"findings":[{"id":"test-1","severity":"high","description":"owner choice","action":"ask-user"}]}`
	executeCalls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		executeCalls++
		if executeCalls == 1 {
			return &StepOutcome{Findings: findings, NeedsApproval: true}, nil
		}
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, paths, nil, nil, []Step{step}, nil)
	privateKey := protectExecutorRun(t, database, exec, run.ID)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
	challenge, err := exec.OwnerDecisionChallenge(ownerdecision.PurposeRespond)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionFix, FindingIDs: []string{"test-1"}})
	if err != nil {
		t.Fatal(err)
	}
	exec.beforeOwnerResume = func() {
		changed := `["test-1","attacker-selected"]`
		if err := database.SetStepRoundUserDecision(challenge.RoundID, &changed, db.RoundSelectionSourceUser, nil); err != nil {
			t.Errorf("inject projection tamper: %v", err)
		}
	}
	if err := exec.RespondAuthorized(envelope); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "owner decision history verification failed") {
			t.Fatalf("executor error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor did not fail closed before signed fix re-execution")
	}
	if executeCalls != 1 {
		t.Fatalf("step executed %d times after tamper", executeCalls)
	}
}

func TestExecutorProtectedRecoveryRequiresExternalExpectedHead(t *testing.T) {
	database, paths, run, repo := setupTest(t)
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
	if _, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, run.HeadSHA, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)

	exec := NewExecutor(database, paths, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	if err := exec.Resume(context.Background(), run, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), "externally supplied expected history head") {
		t.Fatalf("unarmed protected recovery error = %v", err)
	}
	if err := exec.ArmOwnerDecisionHistory(run.ID, ownerdecision.DigestBytes([]byte("rollback-head"))); err == nil {
		t.Fatal("wrong external expected head armed recovery")
	}
	now := time.Now().UTC()
	cancelChallenge := ownerdecision.Challenge{
		Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeCancel,
		RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: authority.InitialHeadSHA, GateHeadSHA: run.HeadSHA,
		PreviousHead: authority.GenesisHead, Nonce: "cancel:" + run.ID + ":" + authority.GenesisHead,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	cancelEnvelope, err := ownerdecision.Sign(privateKey, cancelChallenge, ownerdecision.Response{Action: types.ActionAbort})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := database.AppendOwnerDecision(run.ID, ownerdecision.PurposeCancel+":"+cancelChallenge.Nonce, cancelEnvelope, cancelChallenge, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	recoveredRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := NewExecutor(database, paths, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	if err := recovered.ArmOwnerDecisionHistory(run.ID, appended.Head); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Resume(context.Background(), recoveredRun, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), types.RunCancelReasonAbortedByUser) {
		t.Fatalf("committed cancellation recovery error = %v", err)
	}
	cancelled, err := database.GetRun(run.ID)
	if err != nil || cancelled.Status != types.RunCancelled || cancelled.AwaitingAgentSince != nil {
		t.Fatalf("committed cancellation recovery status=%+v err=%v", cancelled, err)
	}
}

func TestExecutorProtectedRecoveryReplaysCommittedResponseAfterAppendCrash(t *testing.T) {
	database, paths, run, repo := setupTest(t)
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
		RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: authority.InitialHeadSHA, GateHeadSHA: run.HeadSHA,
		Step: types.StepReview, StepResultID: step.ID, RoundID: round.ID,
		FindingsDigest: ownerdecision.DigestBytes([]byte(findings)), PreviousHead: authority.GenesisHead,
		Nonce:    "respond:" + round.ID + ":" + authority.GenesisHead,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	envelope, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := database.AppendOwnerDecision(run.ID, ownerdecision.PurposeRespond+":"+round.ID, envelope, challenge, &db.OwnerDecisionProjection{
		RoundID: round.ID, SelectedFindingIDs: db.DeclinedSelectionJSON, SelectionSource: db.RoundSelectionSourceUserDeclined,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// This is the process-crash boundary: the append is durable, but no
	// approval channel was released and the gate remains parked.
	recoveredRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, paths, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	if err := exec.ArmOwnerDecisionHistory(run.ID, appended.Head); err != nil {
		t.Fatal(err)
	}
	if err := exec.Resume(context.Background(), recoveredRun, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	completed, err := database.GetRun(run.ID)
	if err != nil || completed.Status != types.RunCompleted {
		t.Fatalf("recovered committed response status=%v err=%v", completed, err)
	}
	head, protected, err := database.OwnerDecisionHead(run.ID)
	if err != nil || !protected || head != appended.Head {
		t.Fatalf("recovery appended a second event: head=%s want=%s protected=%v err=%v", head, appended.Head, protected, err)
	}
}

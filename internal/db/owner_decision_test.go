package db

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ownerdecision"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func protectedDecisionFixture(t *testing.T) (*DB, *Run, *StepRound, ed25519.PublicKey, ed25519.PrivateKey, ownerdecision.Challenge) {
	t.Helper()
	database := openTestDB(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/owner-boundary.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/protected", strings.Repeat("1", 40), strings.Repeat("0", 40))
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
	genesisHead, err := ownerdecision.GenesisHeadForRun(publicKey, repo.ID, run.Branch, run.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"high","description":"problem","action":"fix"}]}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	challenge := ownerdecision.Challenge{
		Schema:         ownerdecision.ChallengeSchema,
		Purpose:        ownerdecision.PurposeRespond,
		RunID:          run.ID,
		RepoID:         repo.ID,
		Branch:         run.Branch,
		HeadSHA:        run.HeadSHA,
		GateHeadSHA:    run.HeadSHA,
		Step:           types.StepReview,
		StepResultID:   step.ID,
		RoundID:        round.ID,
		FindingsDigest: ownerdecision.DigestBytes([]byte(findings)),
		PreviousHead:   genesisHead,
		Nonce:          "gate-" + round.ID,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(10 * time.Minute).Unix(),
	}
	return database, run, round, publicKey, privateKey, challenge
}

func TestOwnerDecisionAppendIsTransactionalVerifiableAndIdempotent(t *testing.T) {
	database, run, round, publicKey, privateKey, challenge := protectedDecisionFixture(t)
	now := time.Unix(challenge.IssuedAt, 0).UTC()
	envelope, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	projection := &OwnerDecisionProjection{
		RoundID:            round.ID,
		SelectedFindingIDs: DeclinedSelectionJSON,
		SelectionSource:    RoundSelectionSourceUserDeclined,
	}
	result, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, envelope, challenge, projection, now)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if result.Replay || result.Head == challenge.PreviousHead {
		t.Fatalf("first append = %+v", result)
	}
	envelopeDigest, _ := ownerdecision.EnvelopeDigest(envelope)
	controllerHead, _ := ownerdecision.NextHead(challenge.PreviousHead, envelopeDigest)
	if result.Head != controllerHead {
		t.Fatalf("history head = %s, controller-derived %s", result.Head, controllerHead)
	}
	if err := database.VerifyOwnerDecisionHistory(run.ID, result.Head); err != nil {
		t.Fatalf("verify history: %v", err)
	}
	authority, err := database.GetOwnerDecisionAuthority(run.ID)
	if err != nil || authority == nil || string(authority.PublicKey) != string(publicKey) {
		t.Fatalf("authority = %+v, %v", authority, err)
	}
	stored, err := database.GetRoundsByStep(round.StepResultID)
	if err != nil || len(stored) != 1 || stored[0].SelectionSource == nil || *stored[0].SelectionSource != RoundSelectionSourceUserDeclined {
		t.Fatalf("projected decision = %+v, %v", stored, err)
	}

	replay, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, envelope, challenge, projection, now)
	if err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if !replay.Replay || replay.Head != result.Head {
		t.Fatalf("replay = %+v; want head %s", replay, result.Head)
	}
	var count int
	if err := database.sql.QueryRow(`SELECT COUNT(*) FROM owner_decision_events WHERE run_id = ?`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("event count = %d, %v", count, err)
	}
}

func TestOwnerDecisionHistoryRefusesTamperRollbackReplayAndCrossBinding(t *testing.T) {
	t.Run("projection tamper", func(t *testing.T) {
		database, run, round, _, privateKey, challenge := protectedDecisionFixture(t)
		envelope, _ := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
		projection := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: DeclinedSelectionJSON, SelectionSource: RoundSelectionSourceUserDeclined}
		result, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, envelope, challenge, projection, time.Unix(challenge.IssuedAt, 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.sql.Exec(`UPDATE step_rounds SET selected_finding_ids = '["other"]' WHERE id = ?`, round.ID); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyOwnerDecisionHistory(run.ID, result.Head); err == nil {
			t.Fatal("tampered projection verified")
		}
	})

	t.Run("signed findings tamper", func(t *testing.T) {
		database, run, round, _, privateKey, challenge := protectedDecisionFixture(t)
		envelope, _ := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
		projection := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: DeclinedSelectionJSON, SelectionSource: RoundSelectionSourceUserDeclined}
		result, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, envelope, challenge, projection, time.Unix(challenge.IssuedAt, 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.sql.Exec(`UPDATE step_rounds SET findings_json = '{"findings":[]}' WHERE id = ?`, round.ID); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyOwnerDecisionHistory(run.ID, result.Head); err == nil {
			t.Fatal("tampered signed findings verified")
		}
	})

	t.Run("projection and local record digest rewrite", func(t *testing.T) {
		database, run, round, _, privateKey, challenge := protectedDecisionFixture(t)
		envelope, _ := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionFix, FindingIDs: []string{"review-1"}})
		projection := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: `["review-1"]`, SelectionSource: RoundSelectionSourceUser}
		result, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, envelope, challenge, projection, time.Unix(challenge.IssuedAt, 0))
		if err != nil {
			t.Fatal(err)
		}
		forged := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: `["review-1","other"]`, SelectionSource: RoundSelectionSourceUser}
		forgedDigest, _, err := encodeOwnerDecisionRecord(envelope, forged)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.sql.Exec(
			`UPDATE owner_decision_events SET record_digest = ?, selected_finding_ids = ?, selection_source = ? WHERE run_id = ?`,
			forgedDigest, forged.SelectedFindingIDs, forged.SelectionSource, run.ID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.sql.Exec(
			`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ? WHERE id = ?`,
			forged.SelectedFindingIDs, forged.SelectionSource, round.ID,
		); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyOwnerDecisionHistory(run.ID, result.Head); err == nil {
			t.Fatal("rewritten local projection and digest verified under controller head")
		}
	})

	t.Run("history rollback", func(t *testing.T) {
		database, run, round, _, privateKey, challenge := protectedDecisionFixture(t)
		envelope, _ := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
		projection := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: DeclinedSelectionJSON, SelectionSource: RoundSelectionSourceUserDeclined}
		result, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, envelope, challenge, projection, time.Unix(challenge.IssuedAt, 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.sql.Exec(`DELETE FROM owner_decision_events WHERE run_id = ?`, run.ID); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyOwnerDecisionHistory(run.ID, result.Head); err == nil {
			t.Fatal("rolled-back history verified against external head")
		}
	})

	t.Run("authority replacement", func(t *testing.T) {
		database, run, _, _, _, challenge := protectedDecisionFixture(t)
		attackerPublic, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		attackerKeyID, _ := ownerdecision.KeyID(attackerPublic)
		attackerGenesis, _ := ownerdecision.GenesisHeadForRun(attackerPublic, challenge.RepoID, run.Branch, run.HeadSHA)
		if _, err := database.sql.Exec(
			`UPDATE owner_decision_authorities SET public_key = ?, key_id = ?, genesis_head = ? WHERE run_id = ?`,
			[]byte(attackerPublic), attackerKeyID, attackerGenesis, run.ID,
		); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyOwnerDecisionHistory(run.ID, challenge.PreviousHead); err == nil {
			t.Fatal("replaced public-key authority verified against controller head")
		}
	})

	t.Run("cross run and conflicting replay", func(t *testing.T) {
		database, run, round, _, privateKey, challenge := protectedDecisionFixture(t)
		projection := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: DeclinedSelectionJSON, SelectionSource: RoundSelectionSourceUserDeclined}
		crossRun := challenge
		crossRun.RunID = "different-run"
		envelope, _ := ownerdecision.Sign(privateKey, crossRun, ownerdecision.Response{Action: types.ActionApprove})
		if _, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, envelope, challenge, projection, time.Unix(challenge.IssuedAt, 0)); err == nil {
			t.Fatal("cross-run envelope appended")
		}
		exact, _ := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
		if _, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, exact, challenge, projection, time.Unix(challenge.IssuedAt, 0)); err != nil {
			t.Fatal(err)
		}
		changed := exact.Clone()
		changed.Response.Action = types.ActionSkip
		changed, _ = ownerdecision.Sign(privateKey, challenge, changed.Response)
		if _, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, changed, challenge, projection, time.Unix(challenge.IssuedAt, 0)); err == nil {
			t.Fatal("conflicting gate replay appended")
		}
	})

	t.Run("old exact replay after head advanced", func(t *testing.T) {
		database, run, round, _, privateKey, challenge := protectedDecisionFixture(t)
		now := time.Unix(challenge.IssuedAt, 0)
		projection := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: DeclinedSelectionJSON, SelectionSource: RoundSelectionSourceUserDeclined}
		response, _ := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
		first, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, response, challenge, projection, now)
		if err != nil {
			t.Fatal(err)
		}
		cancelChallenge := ownerdecision.Challenge{
			Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeCancel,
			RunID: run.ID, RepoID: challenge.RepoID, Branch: run.Branch, HeadSHA: challenge.HeadSHA, GateHeadSHA: run.HeadSHA,
			PreviousHead: first.Head, Nonce: "cancel:" + run.ID + ":" + first.Head,
			IssuedAt: challenge.IssuedAt, ExpiresAt: challenge.ExpiresAt,
		}
		cancel, _ := ownerdecision.Sign(privateKey, cancelChallenge, ownerdecision.Response{Action: types.ActionAbort})
		if _, err := database.AppendOwnerDecision(run.ID, "cancel:"+cancelChallenge.Nonce, cancel, cancelChallenge, nil, now); err != nil {
			t.Fatal(err)
		}
		if _, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, response, challenge, projection, now); err == nil {
			t.Fatal("old exact replay rewound the advanced history head")
		}
	})

	t.Run("authorized current head may advance without changing sealed initial head", func(t *testing.T) {
		database, run, round, _, privateKey, challenge := protectedDecisionFixture(t)
		now := time.Unix(challenge.IssuedAt, 0)
		projection := &OwnerDecisionProjection{RoundID: round.ID, SelectedFindingIDs: DeclinedSelectionJSON, SelectionSource: RoundSelectionSourceUserDeclined}
		response, _ := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionApprove})
		first, err := database.AppendOwnerDecision(run.ID, "respond:"+round.ID, response, challenge, projection, now)
		if err != nil {
			t.Fatal(err)
		}
		advancedHead := strings.Repeat("8", 40)
		if err := database.UpdateRunHeadSHA(run.ID, advancedHead); err != nil {
			t.Fatal(err)
		}
		cancelChallenge := ownerdecision.Challenge{
			Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeCancel,
			RunID: run.ID, RepoID: challenge.RepoID, Branch: challenge.Branch,
			HeadSHA: challenge.HeadSHA, GateHeadSHA: advancedHead,
			PreviousHead: first.Head, Nonce: "cancel:" + run.ID + ":" + first.Head,
			IssuedAt: challenge.IssuedAt, ExpiresAt: challenge.ExpiresAt,
		}
		cancel, err := ownerdecision.Sign(privateKey, cancelChallenge, ownerdecision.Response{Action: types.ActionAbort})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.AppendOwnerDecision(run.ID, "cancel:"+cancelChallenge.Nonce, cancel, cancelChallenge, nil, now); err != nil {
			t.Fatalf("advanced current head lost the immutable run binding: %v", err)
		}
	})
}

func TestOwnerDecisionAuthorityIsImmutableAndLegacyRunsRemainUnprotected(t *testing.T) {
	database := openTestDB(t)
	repo, _ := database.InsertRepo(t.TempDir(), "https://example.invalid/repo.git", "main")
	legacy, _ := database.InsertRun(repo.ID, "legacy", "head", "base")
	if authority, err := database.GetOwnerDecisionAuthority(legacy.ID); err != nil || authority != nil {
		t.Fatalf("legacy authority = %+v, %v", authority, err)
	}
	if head, protected, err := database.OwnerDecisionHead(legacy.ID); err != nil || protected || head != "" {
		t.Fatalf("legacy head = %q, protected=%v, err=%v", head, protected, err)
	}

	publicKey, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := database.ProtectRunOwnerDecisions(legacy.ID, publicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ProtectRunOwnerDecisions(legacy.ID, publicKey); err != nil {
		t.Fatalf("exact authority replay: %v", err)
	}
	otherKey, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := database.ProtectRunOwnerDecisions(legacy.ID, otherKey); err == nil {
		t.Fatal("authority replacement succeeded")
	}
	encoded, _ := json.Marshal(publicKey)
	if len(encoded) == 0 {
		t.Fatal("test key did not encode")
	}
}

func TestOwnerDecisionAuthoritySealsImmutableRunIdentity(t *testing.T) {
	mutations := map[string]func(*testing.T, *DB, *Run){
		"repo": func(t *testing.T, database *DB, run *Run) {
			other, err := database.InsertRepo(t.TempDir(), "https://example.invalid/other.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.sql.Exec(`UPDATE runs SET repo_id = ? WHERE id = ?`, other.ID, run.ID); err != nil {
				t.Fatal(err)
			}
		},
		"branch": func(t *testing.T, database *DB, run *Run) {
			if _, err := database.sql.Exec(`UPDATE runs SET branch = ? WHERE id = ?`, "rewritten", run.ID); err != nil {
				t.Fatal(err)
			}
		},
		"initial head": func(t *testing.T, database *DB, run *Run) {
			if _, err := database.sql.Exec(`UPDATE runs SET submitted_head_sha = ? WHERE id = ?`, strings.Repeat("9", 40), run.ID); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			database, run, _, _, _, challenge := protectedDecisionFixture(t)
			mutate(t, database, run)
			if err := database.VerifyOwnerDecisionHistory(run.ID, challenge.PreviousHead); err == nil {
				t.Fatal("rewritten run identity verified against the sealed authority")
			}
		})
	}

	t.Run("authority identity rebind changes external genesis", func(t *testing.T) {
		database, run, _, publicKey, _, challenge := protectedDecisionFixture(t)
		attackerGenesis, err := ownerdecision.GenesisHeadForRun(publicKey, challenge.RepoID, "rewritten", challenge.HeadSHA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.sql.Exec(
			`UPDATE owner_decision_authorities SET branch = ?, genesis_head = ? WHERE run_id = ?`,
			"rewritten", attackerGenesis, run.ID,
		); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyOwnerDecisionHistory(run.ID, challenge.PreviousHead); err == nil {
			t.Fatal("rebound authority verified against controller-held original genesis")
		}
	})
}

func TestProtectedCrashRecoveryFailsIncompletePendingAndProjectsCommittedCancel(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/recovery.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	pending, err := database.InsertRun(repo.ID, "pending", "pending-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	pendingAuthority, err := database.ProtectRunOwnerDecisions(pending.ID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	otherRepo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/rewritten.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(
		`UPDATE runs SET repo_id = ?, branch = ?, head_sha = ?, submitted_head_sha = ? WHERE id = ?`,
		otherRepo.ID, "rewritten", "rewritten-head", "rewritten-head", pending.ID,
	); err != nil {
		t.Fatal(err)
	}

	cancelled, err := database.InsertRun(repo.ID, "cancelled", "cancel-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	cancelAuthority, err := database.ProtectRunOwnerDecisions(cancelled.ID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(cancelled.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	challenge := ownerdecision.Challenge{
		Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeCancel,
		RunID: cancelled.ID, RepoID: repo.ID, Branch: cancelled.Branch,
		HeadSHA: cancelAuthority.InitialHeadSHA, GateHeadSHA: cancelled.HeadSHA,
		PreviousHead: cancelAuthority.GenesisHead, Nonce: "cancel:" + cancelled.ID + ":" + cancelAuthority.GenesisHead,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	envelope, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionAbort})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendOwnerDecision(cancelled.ID, ownerdecision.PurposeCancel+":"+challenge.Nonce, envelope, challenge, nil, now); err != nil {
		t.Fatal(err)
	}

	recovered, err := database.ReconcileProtectedCrashStates()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.PendingFailed != 1 || len(recovered.CancellationRunIDs) != 1 || recovered.CancellationRunIDs[0] != cancelled.ID {
		t.Fatalf("protected crash recovery = %+v", recovered)
	}
	pendingAfter, err := database.GetRun(pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pendingAfter.Status != types.RunFailed || pendingAfter.RepoID != pendingAuthority.RepoID ||
		pendingAfter.Branch != pendingAuthority.Branch || pendingAfter.HeadSHA != pendingAuthority.InitialHeadSHA ||
		pendingAfter.SubmittedHeadSHA == nil || *pendingAfter.SubmittedHeadSHA != pendingAuthority.InitialHeadSHA {
		t.Fatalf("incomplete protected run did not retain sealed identity: %+v", pendingAfter)
	}
	if err := database.RecoverCommittedOwnerCancellation(cancelled.ID); err != nil {
		t.Fatal(err)
	}
	cancelledAfter, err := database.GetRun(cancelled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelledAfter.Status != types.RunCancelled {
		t.Fatalf("committed cancellation recovered as %s", cancelledAfter.Status)
	}
}

func TestRecoveredProtectedRunUsesLatestRoundWithoutRejectingPriorRounds(t *testing.T) {
	database, run, firstRound, _, privateKey, challenge := protectedDecisionFixture(t)
	envelope, err := ownerdecision.Sign(privateKey, challenge, ownerdecision.Response{Action: types.ActionFix, FindingIDs: []string{"review-1"}})
	if err != nil {
		t.Fatal(err)
	}
	projection := &OwnerDecisionProjection{RoundID: firstRound.ID, SelectedFindingIDs: `["review-1"]`, SelectionSource: RoundSelectionSourceUser}
	if _, err := database.AppendOwnerDecision(run.ID, "respond:"+firstRound.ID, envelope, challenge, projection, time.Unix(challenge.IssuedAt, 0)); err != nil {
		t.Fatal(err)
	}
	secondFindings := `{"findings":[{"id":"review-2","severity":"high","description":"next choice","action":"fix"}]}`
	if err := database.SetStepFindings(firstRound.StepResultID, secondFindings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(firstRound.StepResultID, 2, "fix", &secondFindings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRecoveredOwnerDecisionRun(run.ID); err != nil {
		t.Fatalf("prior signed round made the current parked round unrecoverable: %v", err)
	}
}

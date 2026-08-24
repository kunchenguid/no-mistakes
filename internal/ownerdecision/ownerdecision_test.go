package ownerdecision

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testChallenge(now time.Time) Challenge {
	return Challenge{
		Schema:         ChallengeSchema,
		Purpose:        PurposeRespond,
		RunID:          "run-1",
		RepoID:         "repo-1",
		Branch:         "feature/owner-boundary",
		HeadSHA:        "1111111111111111111111111111111111111111",
		GateHeadSHA:    "1111111111111111111111111111111111111111",
		Step:           types.StepReview,
		StepResultID:   "step-1",
		RoundID:        "round-1",
		FindingsDigest: DigestBytes([]byte(`{"findings":[{"id":"review-1"}]}`)),
		PreviousHead:   GenesisHead,
		Nonce:          "gate-round-1",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(10 * time.Minute).Unix(),
	}
}

func TestMaterializeProjectionIsDeterministicFromSignedResponse(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"high","description":"problem","action":"fix"}],"summary":"one","risk_level":"high","risk_rationale":"reason"}`
	response := Response{
		Action:       types.ActionFix,
		FindingIDs:   []string{"review-1"},
		Instructions: map[string]string{"review-1": "preserve the boundary"},
		AddedFindings: []types.Finding{{
			Description: "add coverage",
			Action:      types.ActionAutoFix,
		}},
	}
	first, err := MaterializeProjection(findings, response)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MaterializeProjection(findings, response)
	if err != nil {
		t.Fatal(err)
	}
	if first.SelectedFindingIDs != second.SelectedFindingIDs || first.UserFindingsJSON == nil || second.UserFindingsJSON == nil || *first.UserFindingsJSON != *second.UserFindingsJSON {
		t.Fatalf("projection is not deterministic: %+v vs %+v", first, second)
	}
	var ids []string
	if err := json.Unmarshal([]byte(first.SelectedFindingIDs), &ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "review-1" || ids[1] != "user-1" {
		t.Fatalf("materialized ids = %v", ids)
	}
	projected, err := types.ParseFindingsJSON(*first.UserFindingsJSON)
	if err != nil || len(projected.Items) != 2 || projected.Items[0].UserInstructions != "preserve the boundary" || projected.Items[1].ID != "user-1" {
		t.Fatalf("materialized findings = %+v, %v", projected, err)
	}
}

func TestChallengeValidityWindowHasProtocolMaximum(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	challenge := testChallenge(now)
	challenge.ExpiresAt = now.Add(MaxChallengeLifetime + time.Second).Unix()
	if _, err := Sign(privateKey, challenge, Response{Action: types.ActionApprove}); err == nil {
		t.Fatal("challenge longer than the protocol maximum was signed")
	}
}

func TestSignedDecisionBindsEveryChallengeAndResponseField(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	challenge := testChallenge(now)
	response := Response{
		Action:     types.ActionFix,
		FindingIDs: []string{"review-1"},
		Instructions: map[string]string{
			"review-1": "keep the owner-selected behavior",
		},
	}
	envelope, err := Sign(privateKey, challenge, response)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := Verify(publicKey, envelope, challenge, now); err != nil {
		t.Fatalf("verify exact decision: %v", err)
	}

	tests := map[string]func(*Envelope){
		"cross run":  func(v *Envelope) { v.Challenge.RunID = "run-2" },
		"cross head": func(v *Envelope) { v.Challenge.HeadSHA = "2222222222222222222222222222222222222222" },
		"cross gate head": func(v *Envelope) {
			v.Challenge.GateHeadSHA = "3333333333333333333333333333333333333333"
		},
		"cross gate":    func(v *Envelope) { v.Challenge.RoundID = "round-2" },
		"action":        func(v *Envelope) { v.Response.Action = types.ActionApprove },
		"selection":     func(v *Envelope) { v.Response.FindingIDs = []string{"review-2"} },
		"instructions":  func(v *Envelope) { v.Response.Instructions["review-1"] = "different" },
		"previous head": func(v *Envelope) { v.Challenge.PreviousHead = DigestBytes([]byte("rollback")) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := envelope.Clone()
			mutate(&changed)
			if err := Verify(publicKey, changed, challenge, now); err == nil {
				t.Fatal("mutated envelope verified")
			}
		})
	}
}

func TestSignedDecisionExpiryAndReplayIdentity(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	challenge := testChallenge(now)
	envelope, err := Sign(privateKey, challenge, Response{Action: types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(publicKey, envelope, challenge, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired decision verified")
	}

	firstDigest, err := EnvelopeDigest(envelope)
	if err != nil {
		t.Fatal(err)
	}
	firstHead, err := NextHead(GenesisHead, firstDigest)
	if err != nil {
		t.Fatal(err)
	}
	if firstHead == GenesisHead {
		t.Fatal("history head did not advance")
	}
	if got, err := NextHead(GenesisHead, firstDigest); err != nil || got != firstHead {
		t.Fatalf("exact replay head = %q, %v; want %q", got, err, firstHead)
	}
	if _, err := NextHead(DigestBytes([]byte("different-prefix")), firstDigest); err != nil {
		t.Fatalf("valid alternate prefix should hash deterministically: %v", err)
	}
}

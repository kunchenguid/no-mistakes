package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ownerdecision"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadOwnerRunProtectionBindsPublicKeyAndImmutableRunIdentity(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicEncoded, _ := ownerdecision.EncodePublicKey(publicKey)
	genesisHead, _ := ownerdecision.GenesisHeadForRun(publicKey, "repo-1", "feature", "abc123")
	publicPath := filepath.Join(t.TempDir(), "owner.pub")
	if err := os.WriteFile(publicPath, []byte(publicEncoded), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := loadOwnerRunProtection(publicPath, "", "repo-1", "feature", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.ExpectedHead != genesisHead || config.PublicKey != publicEncoded {
		t.Fatalf("loaded protection = %+v", config)
	}
	if _, err := loadOwnerRunProtection(publicPath, ownerdecision.DigestBytes([]byte("wrong")), "repo-1", "feature", "abc123"); err == nil {
		t.Fatal("non-genesis initial head was accepted")
	}
}

func TestOwnerDecisionSignIsOfflineAndBindsExportedChallenge(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateEncoded, _ := ownerdecision.EncodePrivateKey(privateKey)
	privatePath := filepath.Join(t.TempDir(), "owner.key")
	if err := os.WriteFile(privatePath, []byte(privateEncoded), 0o600); err != nil {
		t.Fatal(err)
	}
	genesisHead, _ := ownerdecision.GenesisHeadForRun(publicKey, "repo-1", "feature", "abc123")
	now := time.Now().UTC()
	challenge := ownerdecision.Challenge{
		Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeRespond,
		RunID: "run-1", RepoID: "repo-1", Branch: "feature", HeadSHA: "abc123", GateHeadSHA: "abc123",
		Step: types.StepReview, StepResultID: "step-1", RoundID: "round-1",
		FindingsDigest: ownerdecision.DigestBytes([]byte(`{"findings":[]}`)), PreviousHead: genesisHead,
		Nonce: "respond:round-1:" + genesisHead, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	challengeBytes, _ := json.Marshal(challenge)
	challengePath := filepath.Join(t.TempDir(), "challenge.json")
	decisionPath := filepath.Join(t.TempDir(), "decision.json")
	if err := os.WriteFile(challengePath, challengeBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newAxiOwnerDecisionSignCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--challenge-file", challengePath, "--private-key", privatePath, "--action", "approve", "--out", decisionPath})
	if runtime.GOOS == "windows" {
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
			t.Fatalf("Windows signer refusal = %v", err)
		}
		return
	}
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	envelope, err := readOwnerDecisionEnvelope(decisionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ownerdecision.Verify(publicKey, envelope, challenge, now); err != nil {
		t.Fatal(err)
	}
	digest, _ := ownerdecision.EnvelopeDigest(envelope)
	nextHead, _ := ownerdecision.NextHead(challenge.PreviousHead, digest)
	if !strings.Contains(output.String(), "next_head: "+nextHead) {
		t.Fatalf("offline signer did not publish controller-derivable next head: %s", output.String())
	}
}

func TestCheckpointSigningKeepsHistoryHeadUnchanged(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateEncoded, _ := ownerdecision.EncodePrivateKey(privateKey)
	privatePath := filepath.Join(t.TempDir(), "owner.key")
	if err := os.WriteFile(privatePath, []byte(privateEncoded), 0o600); err != nil {
		t.Fatal(err)
	}
	historyHead, _ := ownerdecision.GenesisHeadForRun(publicKey, "repo-1", "feature", "abc123")
	now := time.Now().UTC()
	challenge := ownerdecision.Challenge{
		Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeCheckpoint,
		RunID: "run-1", RepoID: "repo-1", Branch: "feature", HeadSHA: "abc123", GateHeadSHA: "abc123",
		PreviousHead: historyHead, Nonce: "checkpoint:run-1:fresh", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	challengeBytes, _ := json.Marshal(challenge)
	challengePath := filepath.Join(t.TempDir(), "checkpoint-challenge.json")
	decisionPath := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := os.WriteFile(challengePath, challengeBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newAxiOwnerDecisionSignCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--challenge-file", challengePath, "--private-key", privatePath, "--out", decisionPath})
	if runtime.GOOS == "windows" {
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
			t.Fatalf("Windows checkpoint signer refusal = %v", err)
		}
		return
	}
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "next_head:") || !strings.Contains(output.String(), "history_head: "+historyHead) {
		t.Fatalf("checkpoint signer implied a journal append: %s", output.String())
	}
}

func TestOwnerPrivateKeyReadRequiresPinnedOwnerOnlyRegularFile(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := ownerdecision.EncodePrivateKey(privateKey)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.key")
	if err := os.WriteFile(good, []byte(encoded+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if _, err := readOwnerPrivateKey(good); err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
			t.Fatalf("Windows private-key refusal = %v", err)
		}
		return
	}
	if loaded, err := readOwnerPrivateKey(good); err != nil || !bytes.Equal(loaded, privateKey) {
		t.Fatalf("secure key read = %v, %v", loaded, err)
	}

	t.Run("symlink", func(t *testing.T) {
		path := filepath.Join(dir, "linked.key")
		if err := os.Symlink(good, path); err != nil {
			t.Fatal(err)
		}
		if _, err := readOwnerPrivateKey(path); err == nil {
			t.Fatal("symlinked private key was accepted")
		}
	})

	t.Run("non-owner-only mode", func(t *testing.T) {
		path := filepath.Join(dir, "shared.key")
		if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := readOwnerPrivateKey(path); err == nil {
			t.Fatal("group-readable private key was accepted")
		}
	})

	t.Run("not regular", func(t *testing.T) {
		if _, err := readOwnerPrivateKey(dir); err == nil {
			t.Fatal("directory private key was accepted")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(dir, "oversized.key")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxOwnerPrivateKeyBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readOwnerPrivateKey(path); err == nil {
			t.Fatal("oversized private key was accepted")
		}
	})
}

func TestOfflineSigningRefusesLongFutureAndExpiredChallenges(t *testing.T) {
	publicKey, _, _ := ed25519.GenerateKey(rand.Reader)
	genesisHead, _ := ownerdecision.GenesisHeadForRun(publicKey, "repo-1", "feature", "abc123")
	now := time.Now().UTC().Truncate(time.Second)
	base := ownerdecision.Challenge{
		Schema: ownerdecision.ChallengeSchema, Purpose: ownerdecision.PurposeCancel,
		RunID: "run-1", RepoID: "repo-1", Branch: "feature", HeadSHA: "abc123", GateHeadSHA: "abc123",
		PreviousHead: genesisHead, Nonce: "cancel:run-1:" + genesisHead,
		IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	tests := map[string]func(*ownerdecision.Challenge){
		"long": func(challenge *ownerdecision.Challenge) {
			challenge.IssuedAt = now.Unix()
			challenge.ExpiresAt = now.Add(ownerdecision.MaxChallengeLifetime + time.Second).Unix()
		},
		"future": func(challenge *ownerdecision.Challenge) {
			challenge.IssuedAt = now.Add(time.Minute).Unix()
			challenge.ExpiresAt = now.Add(2 * time.Minute).Unix()
		},
		"expired": func(challenge *ownerdecision.Challenge) {
			challenge.IssuedAt = now.Add(-2 * time.Minute).Unix()
			challenge.ExpiresAt = now.Add(-time.Minute).Unix()
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			challenge := base
			mutate(&challenge)
			if err := validateChallengeForOfflineSigning(challenge, now); err == nil {
				t.Fatal("unsafe challenge was accepted for offline signing")
			}
		})
	}
}

func TestOwnerDecisionKeygenCreatesPrivate0600AndNeverOverwrites(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "owner.key")
	publicPath := filepath.Join(t.TempDir(), "owner.pub")
	cmd := newAxiOwnerDecisionKeygenCmd()
	cmd.SetArgs([]string{"--private-key", privatePath, "--public-key", publicPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	privateInfo, err := os.Stat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && privateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o", privateInfo.Mode().Perm())
	}
	privateBefore, _ := os.ReadFile(privatePath)
	again := newAxiOwnerDecisionKeygenCmd()
	again.SetArgs([]string{"--private-key", privatePath, "--public-key", publicPath})
	if err := again.Execute(); err == nil {
		t.Fatal("keygen overwrote existing key files")
	}
	privateAfter, _ := os.ReadFile(privatePath)
	if !bytes.Equal(privateBefore, privateAfter) {
		t.Fatal("failed repeat keygen changed the private key")
	}
}

func TestOwnerResponseForSignKeepsYoloAndManualActionsInsideEnvelope(t *testing.T) {
	response, err := ownerResponseForSign(ownerdecision.PurposeRespond, "fix", "review-1", "keep owner behavior", "")
	if err != nil {
		t.Fatal(err)
	}
	if response.Action != types.ActionFix || len(response.FindingIDs) != 1 || response.Instructions["review-1"] != "keep owner behavior" {
		t.Fatalf("response = %+v", response)
	}
	cancel, err := ownerResponseForSign(ownerdecision.PurposeCancel, "", "", "", "")
	if err != nil || cancel.Action != types.ActionAbort {
		t.Fatalf("cancel response = %+v, %v", cancel, err)
	}
	checkpoint, err := ownerResponseForSign(ownerdecision.PurposeCheckpoint, "", "", "", "")
	if err != nil || checkpoint.Action != types.ActionApprove {
		t.Fatalf("checkpoint response = %+v, %v", checkpoint, err)
	}
}

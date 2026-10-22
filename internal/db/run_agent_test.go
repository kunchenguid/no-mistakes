package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunAgentIsImmutableAndReceiptBound(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/agent", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "nonce", "gen", "digest", "", false, nil, types.AgentCodex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec("UPDATE runs SET requested_agent = 'claude' WHERE id = ?", run.ID); err == nil {
		t.Fatal("a persisted run's agent was mutable")
	}
	got, claimed, err := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, types.AgentClaude)
	if err != nil || claimed || got == nil || got.RequestedAgent != types.AgentCodex {
		t.Fatalf("conflicting claim: %+v %v %v", got, claimed, err)
	}
	got, claimed, err = d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, types.AgentCodex)
	if err != nil || !claimed || got.RequestedAgent != types.AgentCodex {
		t.Fatalf("matching claim: %+v %v %v", got, claimed, err)
	}
}

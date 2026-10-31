package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewerSelectionImmutableAndReceiptBound(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentCodex, Model: "review-model", Effort: agentcfg.EffortHigh}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "nonce", "gen", "digest", "", false, nil, pin)
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []any{nil, &agentcfg.ReviewerProfile{Agent: types.AgentPi, Model: "openai/other", Effort: agentcfg.EffortLow}} {
		if _, err := d.sql.Exec(`UPDATE runs SET reviewer_profile = ? WHERE id = ?`, replacement, run.ID); err == nil {
			t.Fatal("reviewer selection could change")
		}
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.ReviewerProfile == nil || *got.ReviewerProfile != *pin {
		t.Fatalf("read reviewer: %+v %v", got, err)
	}
	wrong := &agentcfg.ReviewerProfile{Agent: pin.Agent, Model: "other", Effort: pin.Effort}
	got, claimed, err := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, wrong)
	if err != nil || claimed || got.LaunchReceiptClaimedAt != nil {
		t.Fatalf("conflict consumed receipt: %+v %v %v", got, claimed, err)
	}
	_, claimed, err = d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, &agentcfg.ReviewerProfile{Agent: pin.Agent})
	if err != nil || !claimed {
		t.Fatalf("matching receipt refused: %v %v", claimed, err)
	}
	legacy, err := d.InsertRun(repo.ID, "legacy", "h", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET reviewer_profile = ? WHERE id = ?`, pin, legacy.ID); err == nil {
		t.Fatal("legacy run retroactively pinned")
	}
}

func TestReviewerSelectionMigrationLeavesHistoricalRunsUnpinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "h", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`DROP TRIGGER runs_reviewer_profile_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN reviewer_profile`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	for range 2 {
		d, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.GetRun(run.ID)
		if err != nil || got.ReviewerProfile != nil {
			t.Fatalf("migration changed legacy run: %+v %v", got, err)
		}
		d.Close()
	}
}

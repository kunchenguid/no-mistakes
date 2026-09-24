package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunPiProfileImmutableAndReceiptBound(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	pin := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "nonce", "gen", "digest", "", false, pin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET pi_profile = NULL WHERE id = ?`, run.ID); err == nil {
		t.Fatal("pin could be cleared")
	}
	if _, err := d.sql.Exec(`UPDATE runs SET pi_profile = ? WHERE id = ?`, &agentcfg.PiProfile{Model: "anthropic/other", Effort: agentcfg.EffortLow}, run.ID); err == nil {
		t.Fatal("pin could change")
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.PiProfile == nil || *got.PiProfile != *pin {
		t.Fatalf("read pin: %+v %v", got, err)
	}
	conflict := &agentcfg.PiProfile{Effort: agentcfg.EffortLow}
	got, claimed, err := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, conflict)
	if err != nil || claimed || got.LaunchReceiptClaimedAt != nil {
		t.Fatalf("conflict consumed receipt: %+v %v %v", got, claimed, err)
	}
	_, claimed, err = d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, &agentcfg.PiProfile{Model: pin.Model})
	if err != nil || !claimed {
		t.Fatalf("matching request not claimed: %v %v", claimed, err)
	}
	legacy, err := d.InsertRun(repo.ID, "legacy", "h", "b")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err = d.GetRun(legacy.ID)
	if err != nil || legacy.PiProfile != nil {
		t.Fatalf("legacy pin invented: %+v %v", legacy, err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET pi_profile = ? WHERE id = ?`, pin, legacy.ID); err == nil {
		t.Fatal("legacy run retroactively pinned")
	}
}

func TestOpenMigratesPiProfileWithoutPinningHistoricalRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if _, err := d.sql.Exec(`DROP TRIGGER runs_pi_profile_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN pi_profile`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	for range 2 {
		d, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.GetRun(run.ID)
		if err != nil || got.PiProfile != nil {
			t.Fatalf("migration changed legacy: %+v %v", got, err)
		}
		d.Close()
	}
}

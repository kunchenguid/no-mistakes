package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

func TestRunAgentIsImmutableAndReceiptBound(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, err := d.InsertRunWithAgent(repo.ID, "feature", "head", "base", nil, "nonce", "gen", "digest", "", false, nil, nil, agentcfg.RunAgentCodex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET run_agent = NULL WHERE id = ?`, run.ID); err == nil {
		t.Fatal("selected agent could be cleared")
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("read selection: %+v %v", got, err)
	}
	got, claimed, err := d.ClaimLaunchReceiptWithAgent(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, "", nil)
	if err != nil || claimed || got.LaunchReceiptClaimedAt != nil {
		t.Fatalf("mismatched claim consumed receipt: %+v %v %v", got, claimed, err)
	}
	_, claimed, err = d.ClaimLaunchReceiptWithAgent(repo.ID, "feature", "nonce", "head", "gen", "digest", "", false, agentcfg.RunAgentCodex, nil)
	if err != nil || !claimed {
		t.Fatalf("matching claim failed: %v %v", claimed, err)
	}
}

func TestOpenMigratesRunAgentWithoutSelectingHistoricalRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if _, err := d.sql.Exec(`DROP TRIGGER runs_run_agent_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN run_agent`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil || got.RunAgent != "" {
		t.Fatalf("migration changed historical run: %+v %v", got, err)
	}
}

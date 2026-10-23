package db

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/quotarouting"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func mustJSON(t *testing.T, record *quotarouting.Record) string {
	t.Helper()
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return string(payload)
}

var testTime = time.Unix(1792270231, 0)

// TestRunQuotaRoutingMutableRecord pins the storage contract: unlike the
// immutable pi_profile pin, the quota-routing record is written at run setup
// and AMENDED by mid-run switches, read back through the run row, and a
// pre-feature run's empty payload reads back as "".
func TestRunQuotaRoutingMutableRecord(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.QuotaRouting != "" {
		t.Fatalf("a fresh run carries no record, got %q", got.QuotaRouting)
	}

	record := quotarouting.NewRecord(true,
		[]types.AgentName{types.AgentClaude, types.AgentCodex},
		[]types.AgentName{types.AgentCodex, types.AgentClaude},
		quotarouting.Selection{
			Choice: types.AgentCodex,
			Order:  []types.AgentName{types.AgentCodex, types.AgentClaude},
			Reason: "ranked by spendPriority",
		})
	if err := d.SetRunQuotaRouting(run.ID, mustJSON(t, record)); err != nil {
		t.Fatalf("set: %v", err)
	}

	payload, err := d.GetRunQuotaRouting(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := quotarouting.ParseRecord(payload)
	if err != nil || parsed == nil || parsed.Choice != "codex" || len(parsed.Switches) != 0 {
		t.Fatalf("round trip: %v %+v", err, parsed)
	}

	// The daemon amends the same row on a mid-run switch; the write is a
	// plain overwrite of the opaque JSON, never a fresh insert.
	switched := record.AppendSwitch(types.AgentCodex, types.AgentClaude, "codex exhausted its quota", testTime)
	if err := d.SetRunQuotaRouting(run.ID, mustJSON(t, switched)); err != nil {
		t.Fatalf("amend: %v", err)
	}
	payload, err = d.GetRunQuotaRouting(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = quotarouting.ParseRecord(payload)
	if err != nil || parsed == nil || len(parsed.Switches) != 1 || parsed.Switches[0].To != "claude" {
		t.Fatalf("amended round trip: %v %+v", err, parsed)
	}

	got, err = d.GetRun(run.ID)
	if err != nil || got.QuotaRouting != payload {
		t.Fatalf("the run row scan carries the amended record: %+v %v", got.QuotaRouting, err)
	}
}

func TestStatsIncludeEachRunsPersistedQuotaEvidence(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"version":1,"enabled":true,"configured":["claude","codex"],"order":["codex","claude"],"choice":"codex","reason":"ranked by quota evidence","candidates":[{"agent":"codex","provider":"codex","account":"codex-home","scope":"all_models","remaining":80,"spend_priority":90,"eligible":true,"rankable":true,"reason":"headroom"}],"switches":[{"from":"codex","to":"claude","reason":"quota exceeded","at":123}]}`
	if err := d.SetRunQuotaRouting(run.ID, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, "no-quota", "head", "base"); err != nil {
		t.Fatal(err)
	}
	stats, err := d.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.QuotaRouting) != 1 || stats.QuotaRouting[0].RunID != run.ID {
		t.Fatalf("quota stats = %+v, want only run %s", stats.QuotaRouting, run.ID)
	}
	var want quotarouting.Record
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stats.QuotaRouting[0].Record, &want) {
		t.Fatalf("quota record = %+v, want %+v", stats.QuotaRouting[0].Record, &want)
	}
}

package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLifecycleEventsPreserveFailureAcrossCancellationProjection(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.UpdateRunErrorStatus(run.ID, "codex authorization required", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatus(run.ID, types.RunCancelReasonSuperseded, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	events, err := d.LifecycleEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawFailure, sawCancellation bool
	for _, event := range events {
		if event.EventType == "run_failure" && event.Error == "codex authorization required" {
			sawFailure = true
		}
		if event.Status == string(types.RunCancelled) {
			sawCancellation = true
		}
	}
	if !sawFailure || !sawCancellation {
		t.Fatalf("events lost transition history: %+v", events)
	}
}

func TestRouteDecisionStoresPromptFingerprintWithoutPrompt(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	hash, size := PromptEvidence("secret prompt")
	if err := d.InsertRouteDecision(RouteDecision{RunID: run.ID, RequestedHarness: "codex", EffectiveHarness: "codex", PolicyVersion: "v1", Phase: "review", Reason: "test", PromptSHA256: hash, PromptBytes: size, PromptTransport: "stdin"}); err != nil {
		t.Fatal(err)
	}
	decisions, err := d.RouteDecisions(run.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = %+v, err = %v", decisions, err)
	}
	if decisions[0].PromptSHA256 != hash || decisions[0].PromptBytes != size || decisions[0].PromptTransport != "stdin" {
		t.Fatalf("prompt evidence = %+v", decisions[0])
	}
}

func TestRouteResultsPersistCompletedReviewClassification(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.InsertRouteResult(RouteResult{RunID: run.ID, StepName: "review", Round: 2, Phase: "review-fix", Risk: "low", CreatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	results, err := d.RouteResults(run.ID)
	if err != nil || len(results) != 1 {
		t.Fatalf("route results = %+v, err = %v", results, err)
	}
	if results[0].Phase != "review-fix" || results[0].Risk != "low" || results[0].Round != 2 || results[0].AppendSeq == 0 {
		t.Fatalf("route result = %+v", results[0])
	}
}

func TestRouteResultsUseDurableAppendOrderAcrossClockRollbackAndTies(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	for _, result := range []RouteResult{
		{RunID: run.ID, StepName: "review", Round: 1, Phase: "review-fix", Risk: "high", CreatedAt: 200},
		{RunID: run.ID, StepName: "review", Round: 2, Phase: "review-fix", Risk: "low", CreatedAt: 100},
		{RunID: run.ID, StepName: "review", Round: 3, Phase: "review-fix", Risk: "medium", CreatedAt: 100},
		{RunID: run.ID, StepName: "review", Round: 4, Phase: "review-fix", Risk: "high", CreatedAt: 50},
	} {
		if err := d.InsertRouteResult(result); err != nil {
			t.Fatal(err)
		}
	}
	results, err := d.RouteResults(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"high", "low", "medium", "high"}
	if len(results) != len(want) {
		t.Fatalf("route results = %+v", results)
	}
	for i, risk := range want {
		if results[i].Risk != risk || results[i].AppendSeq != int64(i+1) {
			t.Fatalf("result[%d] = %+v, want risk %q and append sequence %d", i, results[i], risk, i+1)
		}
	}
}

func TestRouteResultsRepairMalformedAndDuplicateAppendSequencesDeterministically(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	for i, risk := range []string{"high", "low", "medium"} {
		if err := d.InsertRouteResult(RouteResult{ID: fmt.Sprintf("repair-%d", i), RunID: run.ID, StepName: "review", Round: i + 1, Phase: "review-fix", Risk: risk, CreatedAt: int64(10 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "repair.sqlite")
	// Copy the test database through SQLite so reopening exercises the same
	// migration/repair path without reaching into DB internals.
	if _, err := d.sql.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP INDEX idx_route_results_append_seq`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE route_results SET append_seq = CASE round WHEN 1 THEN 'bad' WHEN 2 THEN 1 ELSE 1 END`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	repaired, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()
	results, err := repaired.RouteResults(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("repaired results = %+v", results)
	}
	// The first valid sequence wins; malformed and duplicate rows are assigned
	// monotonically after the valid maximum in legacy created_at,id order.
	if results[0].Risk != "low" || results[1].Risk != "high" || results[2].Risk != "medium" {
		t.Fatalf("repaired durable order = %+v", results)
	}
	seqs := make([]int64, 0, len(results))
	for _, result := range results {
		seqs = append(seqs, result.AppendSeq)
	}
	if !sort.SliceIsSorted(seqs, func(i, j int) bool { return seqs[i] < seqs[j] }) || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("repaired sequences = %v", seqs)
	}
}

func TestRouteResultsConcurrentInsertionUsesUniqueMonotonicSequences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := first.InsertRepo("/tmp/concurrent-route-results", "https://github.com/test/concurrent", "main")
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	run, err := first.InsertRun(repo.ID, "feature/concurrent", "head", "base")
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	defer first.Close()
	defer second.Close()

	const writers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		writer := first
		if i%2 == 1 {
			writer = second
		}
		wg.Add(1)
		go func(i int, writer *DB) {
			defer wg.Done()
			errCh <- writer.InsertRouteResult(RouteResult{
				ID: fmt.Sprintf("concurrent-%02d", i), RunID: run.ID, StepName: "review",
				Round: i + 1, Phase: "review-fix", Risk: "low", CreatedAt: 1,
			})
		}(i, writer)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	results, err := first.RouteResults(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != writers {
		t.Fatalf("got %d concurrent results, want %d", len(results), writers)
	}
	for i, result := range results {
		if result.AppendSeq != int64(i+1) {
			t.Fatalf("result[%d] sequence = %d, want %d", i, result.AppendSeq, i+1)
		}
	}
}

func TestProvisioningProjectionIsRestartReadable(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	if err := d.SetRunProvisioning(run.ID, "checkout", 42, ""); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetProvisioningRuns()
	if err != nil || len(got) != 1 {
		t.Fatalf("provisioning runs = %+v, err = %v", got, err)
	}
	if got[0].ProvisioningPhase != "checkout" || got[0].ProvisioningProgress != 42 {
		t.Fatalf("projection = %+v", got[0])
	}
	if err := d.CompleteRunProvisioning(run.ID); err != nil {
		t.Fatal(err)
	}
	gotRun, err := d.GetRun(run.ID)
	if err != nil || gotRun.Status != types.RunPending || gotRun.ProvisioningProgress != 100 {
		t.Fatalf("completed projection = %+v, err = %v", gotRun, err)
	}
}

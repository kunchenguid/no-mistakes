package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCaptureDoesNotCopyRepositoryHistoryPerCase is the regression test for the
// growth that made automatic collection unaffordable: every case used to carry
// a self-contained bundle of the repository's whole history, so a second review
// pass of the same repository cost as much on disk as the first. Two cases from
// one repository must now cost one history plus a delta.
//
// The fixture pads the repository with incompressible history so the two shapes
// are actually distinguishable: with a per-case bundle the second case roughly
// doubles the corpus, and with a shared object pool it adds almost nothing.
func TestCaptureDoesNotCopyRepositoryHistoryPerCase(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, firstRound := setupCapturedRunWithHistory(t, ctx, 24)
	defer sourceDB.Close()

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	afterFirst := dirSize(t, p.EvalDir())
	// A repository whose history is too small to measure would let the old
	// duplicating shape pass this test unnoticed.
	if afterFirst < 128*1024 {
		t.Fatalf("fixture history is too small to detect duplication: corpus = %d bytes", afterFirst)
	}

	addSecondReviewRound(t, ctx, sourceDB, run.ID, repo.WorkingPath, firstRound)
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("captured cases = %d, want 2", len(cases))
	}
	afterSecond := dirSize(t, p.EvalDir())

	marginal := afterSecond - afterFirst
	t.Logf("corpus after first case: %d bytes; marginal cost of second case: %d bytes", afterFirst, marginal)
	if marginal > afterFirst/4 {
		t.Fatalf("second case cost %d bytes on top of %d: history is being copied per case, not shared", marginal, afterFirst)
	}
}

// TestObjectPoolEnablesLongPaths verifies that Git can create the case refs in
// deep Windows temp directories. Git for Windows otherwise uses the legacy
// path limit and fails while locking an otherwise valid ref.
func TestObjectPoolEnablesLongPaths(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	pool := store.poolDir(strings.Repeat("a", 64))
	if err := initializeObjectPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	got, err := git.Run(ctx, pool, "config", "--bool", "core.longpaths")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "true" {
		t.Fatalf("core.longpaths = %q, want true", strings.TrimSpace(got))
	}
}

// TestPruneBoundsTheCorpusOldestFirstAndKeepsEvaluatedCases pins the retention
// contract: automatic collection must not grow without bound, and it must never
// reclaim a case a candidate comparison already depends on.
func TestPruneBoundsTheCorpusOldestFirstAndKeepsEvaluatedCases(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ids := []string{"case-oldest", "case-middle", "case-evaluated", "case-newest"}
	for i, id := range ids {
		seedCase(t, store, id, int64(i))
	}
	if err := store.persistEvaluation(Case{Manifest: Manifest{ID: "case-evaluated"}, Dir: store.caseDir("case-evaluated")}, Evaluation{
		ID: "evaluation", SessionID: "session", CaseID: "case-evaluated", Candidate: "claude+test", Repeat: 1, Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.Prune(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	remaining, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range remaining {
		got = append(got, c.ID)
	}
	want := "case-evaluated case-newest"
	if strings.Join(got, " ") != want {
		t.Fatalf("remaining cases = %v, want %q (oldest dropped, evaluated case protected)", got, want)
	}
	if _, err := os.Stat(store.caseDir("case-oldest")); !os.IsNotExist(err) {
		t.Fatalf("pruned case directory survived: %v", err)
	}
}

// TestPruneKeepsEveryCaseWhenTheCapIsDisabled pins the documented meaning of a
// zero cap, which is the escape hatch for someone building a large corpus on
// purpose.
func TestPruneKeepsCasesReservedByAReplaySession(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, id := range []string{"a", "b", "c"} {
		seedCase(t, store, id, int64(i))
	}
	_, session, err := store.prepareReplay(ctx, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.CaseIDs) != 3 {
		t.Fatalf("reserved cases = %v, want all three cases", session.CaseIDs)
	}
	pruned, err := store.Prune(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 || len(remaining) != 3 {
		t.Fatalf("prune removed %d replay-reserved cases, %d remain", pruned, len(remaining))
	}
	store.releaseReplayReservation(session.ID)
}

func TestPruneReleasesAbandonedReplayReservations(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, id := range []string{"old", "new"} {
		seedCase(t, store, id, int64(i))
	}
	if _, err := store.db.Exec(`INSERT INTO replay_case_reservations (session_id, case_id, reserved_until) VALUES (?, ?, ?)`, "abandoned", "old", 1); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.Prune(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 || len(remaining) != 1 || remaining[0].ID != "new" {
		t.Fatalf("prune removed %d cases and left %#v, want abandoned oldest case removed", pruned, remaining)
	}
}

func TestPruneKeepsEveryCaseWhenTheCapIsDisabled(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, id := range []string{"a", "b", "c"} {
		seedCase(t, store, id, int64(i))
	}
	pruned, err := store.Prune(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 || len(remaining) != 3 {
		t.Fatalf("prune with disabled cap removed %d cases, %d remain; want 0 removed and 3 remaining", pruned, len(remaining))
	}
}

// TestDropCaseObjectsReleasesOnlyItsOwnPins proves retention reclaims the right
// objects: one case leaving the corpus must not strip the pins another case
// still replays from.
func TestConcurrentCaptureKeepsThePublishedCaseRestorable(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	const workers = 8
	stores := make([]*Store, workers)
	for i := range stores {
		store, err := Open(p.EvalDir())
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = store
		defer store.Close()
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			<-start
			_, err := Capture(ctx, store, p, sourceDB, run.ID)
			errs <- err
		}(store)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent capture failed: %v", err)
		}
	}

	cases, err := stores[0].ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("captured cases = %d, want 1", len(cases))
	}
	restored := filepath.Join(t.TempDir(), "restore.git")
	if err := git.InitBare(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := restoreCaseObjects(ctx, stores[0].poolDir(cases[0].RepoFingerprint), restored, cases[0].ID); err != nil {
		t.Fatalf("concurrently published case is not restorable: %v", err)
	}
}

func TestCaptureReconcilesPendingDeletionBeforeRecapturing(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := cases[0]

	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO pending_case_deletions (id, path, repo_fingerprint) VALUES (?, ?, ?)`, c.ID, c.Dir, c.RepoFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM cases WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	recaptured, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recaptured) != 1 || recaptured[0].ID != c.ID {
		t.Fatalf("recaptured cases = %#v, want %q", recaptured, c.ID)
	}
	if _, err := store.Prune(ctx, 0); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != c.ID {
		t.Fatalf("remaining cases = %#v, want recaptured case %q", remaining, c.ID)
	}
	restored := filepath.Join(t.TempDir(), "restore.git")
	if err := git.InitBare(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := restoreCaseObjects(ctx, store.poolDir(c.RepoFingerprint), restored, c.ID); err != nil {
		t.Fatalf("recaptured case is not restorable: %v", err)
	}
}

func TestDropCaseObjectsRemovesPoolAfterLastCase(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool := store.poolDir(cases[0].RepoFingerprint)

	if err := dropCaseObjects(ctx, pool, cases[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pool); !os.IsNotExist(err) {
		t.Fatalf("empty object pool survived last case deletion: %v", err)
	}
}

func TestDropCaseObjectsReleasesOnlyItsOwnPins(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	addSecondReviewRound(t, ctx, sourceDB, run.ID, repo.WorkingPath, firstRound)
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool := store.poolDir(cases[0].RepoFingerprint)

	if err := dropCaseObjects(ctx, pool, cases[0].ID); err != nil {
		t.Fatal(err)
	}
	refs := mustGit(t, ctx, pool, "for-each-ref", "--format=%(refname)", caseRefNamespace)
	if strings.Contains(refs, cases[0].ID) {
		t.Fatalf("dropped case still pins objects: %s", refs)
	}
	if !strings.Contains(refs, cases[1].ID) {
		t.Fatalf("surviving case lost its pins: %s", refs)
	}
	restored := filepath.Join(t.TempDir(), "restore.git")
	if err := git.InitBare(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := restoreCaseObjects(ctx, pool, restored, cases[1].ID); err != nil {
		t.Fatalf("surviving case is no longer restorable: %v", err)
	}
}

func seedCase(t *testing.T, store *Store, id string, capturedAt int64) {
	t.Helper()
	dir := store.caseDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "labels.json"), Labels{Version: labelsVersion}); err != nil {
		t.Fatal(err)
	}
	c := Case{
		Manifest: Manifest{Version: manifestVersion, ID: id, SourceRunID: "run-" + id, SourceRoundID: "round-" + id, CapturedAt: capturedAt, RepoFingerprint: "fingerprint"},
		Dir:      dir,
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), c.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "original"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "original", "decision.json"), Decision{}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "original", "baseline.json"), BaselineMetrics{}); err != nil {
		t.Fatal(err)
	}
	if err := store.registerCase(c); err != nil {
		t.Fatal(err)
	}
}

// padHistory appends commits of incompressible content so the fixture repo has
// history worth duplicating.
func padHistory(t *testing.T, ctx context.Context, workDir string, commits int) {
	t.Helper()
	if commits <= 0 {
		return
	}
	for i := 0; i < commits; i++ {
		blob := make([]byte, 8*1024)
		if _, err := rand.Read(blob); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("padding-%02d.bin", i)
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(hex.EncodeToString(blob)), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, ctx, workDir, "add", name)
		mustGit(t, ctx, workDir, "commit", "-m", "padding "+name)
	}
	mustGit(t, ctx, workDir, "push", "origin", "HEAD")
}

func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

// addSecondReviewRound appends a fix round to the fixture run so one run yields
// two cases from the same repository.
func addSecondReviewRound(t *testing.T, ctx context.Context, sourceDB *db.DB, runID, workDir string, firstRound *db.StepRound) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n\nfunc Fixed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", "main.go")
	mustGit(t, ctx, workDir, "commit", "-m", "fix review finding")
	mustGit(t, ctx, workDir, "push", "origin", "feature/eval")
	fixedSHA := mustGit(t, ctx, workDir, "rev-parse", "HEAD")
	steps, err := sourceDB.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"fixed","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", &clean, nil, fixedSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25); err != nil {
		t.Fatal(err)
	}
}

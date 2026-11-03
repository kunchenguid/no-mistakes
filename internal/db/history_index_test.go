package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// historyIndexes are the run-history lookup indexes and their key columns in
// order. A missing one, or one with a different column order, puts a full
// history scan back on the daemon's single connection.
var historyIndexes = map[string][]string{
	"idx_step_results_run_order_id":   {"run_id", "step_order", "id"},
	"idx_step_rounds_step_round":      {"step_result_id", "round"},
	"idx_runs_repo_branch_created_id": {"repo_id", "branch", "created_at", "id"},
	"idx_runs_status_created_id":      {"status", "created_at", "id"},
}

// seedHistory writes two runs on one branch, each with a review step and
// rounds, one of them carrying a recorded user decision, so every history
// reader below has rows to return.
func seedHistory(t *testing.T, d *DB) (repoID, branch string, current *Run, review *StepResult) {
	t.Helper()
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, _, decided := seedRound(t, d, repo.ID, "feature", types.StepReview)
	if err := d.SetStepRoundDeclined(decided.ID); err != nil {
		t.Fatal(err)
	}
	current, review, _ = seedRound(t, d, repo.ID, "feature", types.StepReview)
	if _, err := d.InsertStepResult(current.ID, types.StepTest); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(current.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	return repo.ID, "feature", current, review
}

// dropHistoryIndexes turns a migrated database back into one written before
// the history indexes existed.
func dropHistoryIndexes(t *testing.T, d *DB) {
	t.Helper()
	for name := range historyIndexes {
		if _, err := d.sql.Exec(`DROP INDEX IF EXISTS ` + name); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
}

func indexColumns(t *testing.T, d *DB, name string) []string {
	t.Helper()
	rows, err := d.sql.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cols
}

func assertHistoryIndexes(t *testing.T, d *DB) {
	t.Helper()
	for name, want := range historyIndexes {
		if got := indexColumns(t, d, name); !reflect.DeepEqual(got, want) {
			t.Errorf("index %s columns = %v, want %v", name, got, want)
		}
	}
}

// schemaAndRows is everything an idempotent migration must leave unchanged
// once it has run: the schema text and the row count of every table.
func schemaAndRows(t *testing.T, d *DB) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	var tables []string
	rows, err := d.sql.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name, text string
		if err := rows.Scan(&typ, &name, &text); err != nil {
			t.Fatal(err)
		}
		snapshot[typ+" "+name] = text
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	rows.Close()
	for _, table := range tables {
		var n int
		if err := d.sql.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		snapshot["rows "+table] = fmt.Sprint(n)
	}
	return snapshot
}

func assertConsistent(t *testing.T, d *DB) {
	t.Helper()
	var integrity string
	if err := d.sql.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
	rows, err := d.sql.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation")
	}
}

func TestOpenCreatesHistoryIndexesOnAFreshDatabase(t *testing.T) {
	assertHistoryIndexes(t, openTestDB(t))
}

// A database written before the history indexes existed gains them on its
// next writable open, keeps every row, and later opens change nothing.
func TestOpenAddsHistoryIndexesToAnExistingDatabaseOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repoID, branch, current, review := seedHistory(t, d)
	dropHistoryIndexes(t, d)
	before := schemaAndRows(t, d)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// The read-only open used before a command is classified never migrates,
	// and the history readers still work against the unindexed schema.
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if steps, err := ro.GetStepsByRun(current.ID); err != nil || len(steps) != 2 {
		t.Fatalf("read-only steps = %d, %v", len(steps), err)
	}
	if rounds, err := ro.GetRoundsByStep(review.ID); err != nil || len(rounds) != 1 {
		t.Fatalf("read-only rounds = %d, %v", len(rounds), err)
	}
	if prev, err := ro.GetPreviousRunReviewRounds(repoID, branch, current.ID); err != nil || prev == nil {
		t.Fatalf("read-only previous review = %v, %v", prev, err)
	}
	if decisions, _, err := ro.GetBranchDecisionRounds(repoID, branch, current.ID, 0); err != nil || len(decisions) != 1 {
		t.Fatalf("read-only branch decisions = %d, %v", len(decisions), err)
	}
	if got := schemaAndRows(t, ro); !reflect.DeepEqual(got, before) {
		t.Fatalf("read-only open changed the schema or rows:\n got %v\nwant %v", got, before)
	}
	_ = ro.Close()

	var migrated map[string]string
	for open := 1; open <= 3; open++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", open, err)
		}
		assertHistoryIndexes(t, d)
		assertConsistent(t, d)
		got := schemaAndRows(t, d)
		for key, value := range before {
			if strings.HasPrefix(key, "rows ") && got[key] != value {
				t.Errorf("open %d: %s = %s, want %s", open, key, got[key], value)
			}
		}
		if open == 1 {
			migrated = got
		} else if !reflect.DeepEqual(got, migrated) {
			t.Errorf("open %d changed an already migrated database:\n got %v\nwant %v", open, got, migrated)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// queryRecorder captures every query a DB method sends, with its arguments,
// so the test plans the exact statements production code runs rather than a
// copy of them that could drift.
type queryRecorder struct {
	mu      sync.Mutex
	queries []recordedQuery
}

type recordedQuery struct {
	sql  string
	args []any
}

func (r *queryRecorder) take() []recordedQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.queries
	r.queries = nil
	return out
}

type recordingConnector struct {
	inner driver.Driver
	dsn   string
	rec   *queryRecorder
}

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.inner.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("sqlite driver connection %T does not implement QueryerContext", conn)
	}
	return recordingConn{Conn: conn, queryer: queryer, rec: c.rec}, nil
}

func (c recordingConnector) Driver() driver.Driver { return c.inner }

type recordingConn struct {
	driver.Conn
	queryer driver.QueryerContext
	rec     *queryRecorder
}

func (c recordingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	c.rec.mu.Lock()
	c.rec.queries = append(c.rec.queries, recordedQuery{sql: query, args: values})
	c.rec.mu.Unlock()
	return c.queryer.QueryContext(ctx, query, args)
}

// historyScan matches a query-plan step that walks a whole run-history table,
// by name or by the aliases the branch joins use (r, res, sr). SEARCH steps,
// temporary sorts, and scans of other tables are all allowed: the regression
// this guards is a lookup going back to reading the entire history.
var historyScan = regexp.MustCompile(`^SCAN (runs|step_results|step_rounds|r|res|sr)\b`)

// historyScans runs fn against the database at path, then plans every query
// it sent and returns the plan steps that scan a whole history table.
func historyScans(t *testing.T, path string, fn func(*DB) error) []string {
	t.Helper()
	probe, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	rec := &queryRecorder{}
	recorded := sql.OpenDB(recordingConnector{inner: probe.Driver(), dsn: path, rec: rec})
	recorded.SetMaxOpenConns(1)
	d := &DB{sql: recorded}
	defer d.Close()
	if err := fn(d); err != nil {
		t.Fatal(err)
	}
	queries := rec.take()
	if len(queries) == 0 {
		t.Fatal("lookup sent no queries")
	}
	var scans []string
	for _, q := range queries {
		rows, err := probe.Query(`EXPLAIN QUERY PLAN `+q.sql, q.args...)
		if err != nil {
			t.Fatalf("explain %q: %v", q.sql, err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if historyScan.MatchString(detail) {
				scans = append(scans, detail+"  <-  "+q.sql)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return scans
}

// Every run-history lookup on the daemon's hot paths must use the history
// indexes rather than scan the run, step, or round history. Each lookup is
// also planned against the same database with the indexes dropped, which
// must scan: that proves the lookup depends on these indexes, so the test
// fails if one is removed or a query changes shape so it can no longer use
// them.
func TestHistoryLookupsUseHistoryIndexes(t *testing.T) {
	dir := t.TempDir()
	indexed := filepath.Join(dir, "indexed.sqlite")
	unindexed := filepath.Join(dir, "unindexed.sqlite")

	d, err := Open(indexed)
	if err != nil {
		t.Fatal(err)
	}
	repoID, branch, current, review := seedHistory(t, d)
	if _, err := d.sql.Exec(`VACUUM INTO ?`, unindexed); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", unindexed)
	if err != nil {
		t.Fatal(err)
	}
	dropHistoryIndexes(t, &DB{sql: raw})
	_ = raw.Close()

	lookups := []struct {
		name string
		fn   func(*DB) error
	}{
		{"steps by run", func(d *DB) error { _, err := d.GetStepsByRun(current.ID); return err }},
		{"rounds by step", func(d *DB) error { _, err := d.GetRoundsByStep(review.ID); return err }},
		{"previous review on branch", func(d *DB) error {
			_, err := d.GetPreviousRunReviewRounds(repoID, branch, current.ID)
			return err
		}},
		{"branch decisions", func(d *DB) error {
			_, _, err := d.GetBranchDecisionRounds(repoID, branch, current.ID, 0)
			return err
		}},
		{"active run on branch", func(d *DB) error { _, err := d.GetActiveRun(repoID, branch); return err }},
		{"active run in repo", func(d *DB) error { _, err := d.GetActiveRun(repoID, ""); return err }},
		{"runs by repo", func(d *DB) error { _, err := d.GetRunsByRepo(repoID); return err }},
		{"runs by repo head", func(d *DB) error { _, err := d.GetRunsByRepoHead(repoID, branch, "head"); return err }},
		{"active runs", func(d *DB) error { _, err := d.GetActiveRuns(); return err }},
		{"active run worktrees", func(d *DB) error { _, err := d.ActiveRunWorktrees(); return err }},
	}
	for _, lookup := range lookups {
		t.Run(lookup.name, func(t *testing.T) {
			if scans := historyScans(t, indexed, lookup.fn); len(scans) != 0 {
				t.Errorf("lookup scans run history despite the indexes:\n%v", scans)
			}
			if scans := historyScans(t, unindexed, lookup.fn); len(scans) == 0 {
				t.Error("lookup does not scan history even without the indexes, so this test cannot detect their loss")
			}
		})
	}
}

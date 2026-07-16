package db

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// DB wraps a SQLite database connection.
type DB struct {
	sql *sql.DB
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	tx, err := sqlDB.Begin()
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("begin migration: %w", err)
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		_ = tx.Rollback()
		sqlDB.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	for _, stmt := range migrationStatements {
		if _, err := tx.Exec(stmt); err != nil && !isDuplicateColumnErr(err) {
			_ = tx.Rollback()
			sqlDB.Close()
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}
	needsRepair, err := routeResultSequenceNeedsRepair(tx)
	if err != nil {
		_ = tx.Rollback()
		sqlDB.Close()
		return nil, fmt.Errorf("migrate db: inspect route result sequence authority: %w", err)
	}
	if needsRepair {
		if err := repairRouteResultAppendSequences(tx); err != nil {
			_ = tx.Rollback()
			sqlDB.Close()
			return nil, fmt.Errorf("migrate db: %w", err)
		}
		if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_route_results_append_seq ON route_results (append_seq)`); err != nil {
			_ = tx.Rollback()
			sqlDB.Close()
			return nil, fmt.Errorf("migrate db: create route result append index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("commit migration: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// routeResultSequenceNeedsRepair keeps the migration's write-heavy repair
// path one-time. Once both the unique index and its authority row exist,
// normal DB opens must not rewrite route_results or contend with active
// writers merely to re-check an already repaired database.
func routeResultSequenceNeedsRepair(tx *sql.Tx) (bool, error) {
	var indexCount int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_route_results_append_seq'`).Scan(&indexCount); err != nil {
		return false, err
	}
	var authorityCount int
	if err := tx.QueryRow(`SELECT count(*) FROM route_result_sequence WHERE id = 1`).Scan(&authorityCount); err != nil {
		return false, err
	}
	return indexCount == 0 || authorityCount == 0, nil
}

// repairRouteResultAppendSequences upgrades legacy route-result rows to a
// durable insertion order before the unique index is installed. Existing
// positive integer sequences are preserved on their first occurrence; null,
// malformed, non-positive, and duplicate values are reassigned after the
// valid maximum in the old created_at/id order.
func repairRouteResultAppendSequences(tx *sql.Tx) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO route_result_sequence (id, next_seq) VALUES (1, 0)`); err != nil {
		return fmt.Errorf("initialize route result sequence: %w", err)
	}
	rows, err := tx.Query(`SELECT id, CAST(append_seq AS TEXT)
		FROM route_results
		ORDER BY CASE WHEN typeof(created_at) IN ('integer','real') THEN 0 ELSE 1 END,
		         CASE WHEN typeof(created_at) IN ('integer','real') THEN CAST(created_at AS INTEGER) ELSE 0 END,
		         id`)
	if err != nil {
		return fmt.Errorf("read route result append sequences: %w", err)
	}
	type routeResultSequence struct {
		id  string
		seq string
	}
	var ordered []routeResultSequence
	for rows.Next() {
		var item routeResultSequence
		var seq sql.NullString
		if err := rows.Scan(&item.id, &seq); err != nil {
			rows.Close()
			return fmt.Errorf("scan route result append sequence: %w", err)
		}
		if seq.Valid {
			item.seq = seq.String
		}
		ordered = append(ordered, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close route result append sequence rows: %w", err)
	}

	seen := make(map[int64]struct{}, len(ordered))
	assigned := make(map[string]int64, len(ordered))
	var maxSeq int64
	for i := range ordered {
		seq, ok := parsePositiveSequence(ordered[i].seq)
		if !ok {
			continue
		}
		if _, duplicate := seen[seq]; duplicate {
			continue
		}
		seen[seq] = struct{}{}
		assigned[ordered[i].id] = seq
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	for _, item := range ordered {
		if _, ok := assigned[item.id]; ok {
			continue
		}
		if maxSeq == math.MaxInt64 {
			return fmt.Errorf("route result sequence exhausted while repairing %q", item.id)
		}
		maxSeq++
		assigned[item.id] = maxSeq
	}
	for _, item := range ordered {
		if _, err := tx.Exec(`UPDATE route_results SET append_seq = ? WHERE id = ?`, assigned[item.id], item.id); err != nil {
			return fmt.Errorf("repair route result append sequence: %w", err)
		}
	}
	var current int64
	if err := tx.QueryRow(`SELECT next_seq FROM route_result_sequence WHERE id = 1`).Scan(&current); err != nil {
		return fmt.Errorf("read route result sequence authority: %w", err)
	}
	if current < maxSeq {
		if _, err := tx.Exec(`UPDATE route_result_sequence SET next_seq = ? WHERE id = 1`, maxSeq); err != nil {
			return fmt.Errorf("update route result sequence authority: %w", err)
		}
	}
	return nil
}

func parsePositiveSequence(raw string) (int64, bool) {
	seq, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return seq, err == nil && seq > 0
}

// isDuplicateColumnErr reports whether err is SQLite's "duplicate column name"
// error, which ALTER TABLE ADD COLUMN emits when the column already exists.
// Treating this as a no-op keeps migrations idempotent without a version table.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.sql.Close()
}

// newID generates a new ULID with monotonic ordering.
func newID() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// now returns the current unix timestamp in seconds.
func now() int64 {
	return time.Now().Unix()
}

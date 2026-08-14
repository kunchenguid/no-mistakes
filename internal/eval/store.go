package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the opt-in sqlite-first local registry. It is intentionally a
// separate database so merely opening the normal pipeline DB never creates an
// eval table or runs an eval migration.
type Store struct {
	root  string
	cases string
	db    *sql.DB
}

// Open creates the local eval registry. Nothing calls this outside an explicit
// eval subcommand.
func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("eval root is empty")
	}
	root = filepath.Clean(root)
	cases := filepath.Join(root, "cases")
	if err := os.MkdirAll(cases, 0o755); err != nil {
		return nil, fmt.Errorf("create eval cases directory: %w", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(root, "registry.sqlite")+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open eval registry: %w", err)
	}
	database.SetMaxOpenConns(1)
	store := &Store{root: root, cases: cases, db: database}
	if err := store.migrate(); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("eval registry is closed")
	}
	if err := s.dropParkPassSchema(); err != nil {
		return err
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS cases (
    id TEXT PRIMARY KEY,
    source_run_id TEXT NOT NULL,
    source_round_id TEXT NOT NULL UNIQUE,
    captured_at INTEGER NOT NULL,
    repo_fingerprint TEXT NOT NULL,
    branch TEXT NOT NULL,
    language TEXT NOT NULL,
    size_bucket TEXT NOT NULL,
    severity TEXT NOT NULL,
    gold_count INTEGER NOT NULL,
    path TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS pending_case_deletions (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL,
    repo_fingerprint TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS replay_case_reservations (
    session_id TEXT NOT NULL,
    case_id TEXT NOT NULL REFERENCES cases(id) ON DELETE RESTRICT,
    reserved_until INTEGER NOT NULL,
    PRIMARY KEY (session_id, case_id)
);
CREATE TABLE IF NOT EXISTS evaluations (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    case_id TEXT NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    candidate TEXT NOT NULL,
    repeat_number INTEGER NOT NULL,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL,
    status TEXT NOT NULL,
    gold_count INTEGER NOT NULL,
    true_positive INTEGER NOT NULL,
    false_negative INTEGER NOT NULL,
    false_positive INTEGER NOT NULL,
    pending INTEGER NOT NULL,
    tokens_reported INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    fresh_input_tokens INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    path TEXT NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_eval_evaluations_candidate ON evaluations(candidate, completed_at, id);
CREATE INDEX IF NOT EXISTS idx_eval_evaluations_case ON evaluations(case_id, completed_at, id);
`)
	if err != nil {
		return fmt.Errorf("migrate eval registry: %w", err)
	}
	var reservationLeaseColumn int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('replay_case_reservations') WHERE name = 'reserved_until'`).Scan(&reservationLeaseColumn); err != nil {
		return fmt.Errorf("inspect eval replay reservation schema: %w", err)
	}
	if reservationLeaseColumn == 0 {
		if _, err := s.db.Exec(`ALTER TABLE replay_case_reservations ADD COLUMN reserved_until INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate eval replay reservations: %w", err)
		}
	}
	return nil
}

func (s *Store) dropParkPassSchema() error {
	var parkColumn int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('cases') WHERE name = 'verdict_should_park'`).Scan(&parkColumn); err != nil {
		return fmt.Errorf("inspect eval case schema: %w", err)
	}
	if parkColumn == 0 {
		return nil
	}
	_, err := s.db.Exec(`
DROP TABLE IF EXISTS evaluations;
DROP TABLE IF EXISTS replay_case_reservations;
DROP TABLE IF EXISTS pending_case_deletions;
DROP TABLE IF EXISTS cases;
`)
	if err != nil {
		return fmt.Errorf("replace park/pass eval registry: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) caseDir(id string) string { return filepath.Join(s.cases, id) }

func (s *Store) registerCase(c Case) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("eval registry is closed")
	}
	language, size, severity := caseComposition(c)
	_, err := s.db.Exec(`INSERT OR IGNORE INTO cases
(id, source_run_id, source_round_id, captured_at, repo_fingerprint, branch, language, size_bucket, severity, gold_count, path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.SourceRunID, c.SourceRoundID, c.CapturedAt, c.RepoFingerprint, c.Branch, language, size, severity, len(c.Labels.Findings), c.Dir)
	if err != nil {
		return fmt.Errorf("register eval case: %w", err)
	}
	return nil
}

// ListCases resolves the three logical sets. Diversified is deterministic:
// it retains the lexicographically first case in each repo/language/size/gold
// bucket, making its composition visible and stable before a user spends tokens.
func (s *Store) ListCases(set string) ([]Case, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("eval registry is closed")
	}
	set = strings.TrimSpace(strings.ToLower(set))
	if set == "" {
		set = "all"
	}
	if set != "all" && set != "labeled" && set != "diversified" {
		return nil, fmt.Errorf("unknown case set %q (use all, labeled, or diversified)", set)
	}
	rows, err := s.db.Query(`SELECT id, path FROM cases ORDER BY captured_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list eval cases: %w", err)
	}
	defer rows.Close()
	var all []Case
	for rows.Next() {
		var id, dir string
		if err := rows.Scan(&id, &dir); err != nil {
			return nil, fmt.Errorf("scan eval case: %w", err)
		}
		c, err := loadCase(dir)
		if err != nil {
			return nil, fmt.Errorf("load eval case %s: %w", id, err)
		}
		all = append(all, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list eval cases: %w", err)
	}

	switch set {
	case "all":
		return all, nil
	case "labeled":
		out := make([]Case, 0, len(all))
		for _, c := range all {
			if c.Labels.HasGold() {
				out = append(out, c)
			}
		}
		return out, nil
	case "diversified":
		seen := make(map[string]bool, len(all))
		out := make([]Case, 0, len(all))
		for _, c := range all {
			key := diversifiedKey(c)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown case set %q", set)
	}
}

// Prune bounds the corpus at maxCases by dropping the oldest cases first, and
// reports how many it removed. A maxCases of 0 or less keeps every case.
//
// It never removes a case reserved by a replay session or one that already has
// recorded candidate replays: those evaluations are the result of tokens
// somebody spent, and a cohort in an eval report pins the exact case IDs it
// compared, so reclaiming one would silently invalidate a published comparison.
// When protected cases alone exceed the cap the corpus stays over it rather than
// deleting that evidence - the cap is a retention target, not a promise to
// reach a number.
func (s *Store) Prune(ctx context.Context, maxCases int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("eval registry is closed")
	}
	unlock, err := lockCorpus(ctx, s.root)
	if err != nil {
		return 0, err
	}
	defer unlock()

	if err := s.cleanupPendingCaseDeletions(ctx); err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM replay_case_reservations WHERE reserved_until <= ?`, time.Now().Unix()); err != nil {
		return 0, fmt.Errorf("release abandoned eval replay reservations: %w", err)
	}
	if maxCases <= 0 {
		return 0, nil
	}
	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM cases`).Scan(&total); err != nil {
		return 0, fmt.Errorf("count eval cases: %w", err)
	}
	excess := total - maxCases
	if excess <= 0 {
		return 0, nil
	}
	rows, err := s.db.Query(`SELECT c.id, c.path, c.repo_fingerprint FROM cases c
WHERE NOT EXISTS (SELECT 1 FROM evaluations e WHERE e.case_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM replay_case_reservations r WHERE r.case_id = c.id AND r.reserved_until > ?)
ORDER BY c.captured_at, c.id LIMIT ?`, time.Now().Unix(), excess)
	if err != nil {
		return 0, fmt.Errorf("select prunable eval cases: %w", err)
	}
	type victim struct{ id, dir, fingerprint string }
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.dir, &v.fingerprint); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan prunable eval case: %w", err)
		}
		victims = append(victims, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("select prunable eval cases: %w", err)
	}
	rows.Close()

	pruned := 0
	for _, v := range victims {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return pruned, fmt.Errorf("begin eval case prune: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO pending_case_deletions (id, path, repo_fingerprint) VALUES (?, ?, ?)`, v.id, v.dir, v.fingerprint); err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM cases WHERE id = ?`, v.id)
		}
		if err != nil {
			_ = tx.Rollback()
			return pruned, fmt.Errorf("stage eval case %q deletion: %w", v.id, err)
		}
		if err := tx.Commit(); err != nil {
			return pruned, fmt.Errorf("commit eval case %q deletion: %w", v.id, err)
		}
		pruned++
	}
	if err := s.cleanupPendingCaseDeletions(ctx); err != nil {
		return pruned, err
	}
	return pruned, nil
}

func (s *Store) cleanupPendingCaseDeletions(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, repo_fingerprint FROM pending_case_deletions ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list pending eval case deletions: %w", err)
	}
	type pending struct{ id, dir, fingerprint string }
	var items []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.dir, &item.fingerprint); err != nil {
			rows.Close()
			return fmt.Errorf("scan pending eval case deletion: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list pending eval case deletions: %w", err)
	}
	rows.Close()
	for _, item := range items {
		if err := dropCaseObjects(ctx, s.poolDir(item.fingerprint), item.id); err != nil {
			return err
		}
		if err := os.RemoveAll(item.dir); err != nil {
			return fmt.Errorf("remove eval case %q: %w", item.id, err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_case_deletions WHERE id = ?`, item.id); err != nil {
			return fmt.Errorf("finish eval case %q deletion: %w", item.id, err)
		}
	}
	return nil
}

func loadCase(dir string) (Case, error) {
	var manifest Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &manifest); err != nil {
		return Case{}, err
	}
	if manifest.Version != manifestVersion {
		return Case{}, fmt.Errorf("unsupported case manifest version %d (captured by an older no-mistakes whose case format is no longer readable; remove the eval directory to start a fresh corpus, which now refills itself)", manifest.Version)
	}
	var labels Labels
	if err := readJSON(filepath.Join(dir, "labels.json"), &labels); err != nil {
		return Case{}, err
	}
	if labels.Version != labelsVersion {
		return Case{}, fmt.Errorf("unsupported case labels version %d (finding-level gold replaced the park/pass verdict; remove the eval directory to start a fresh corpus)", labels.Version)
	}
	var decision Decision
	if err := readJSON(filepath.Join(dir, "original", "decision.json"), &decision); err != nil {
		return Case{}, err
	}
	var baseline BaselineMetrics
	if err := readJSON(filepath.Join(dir, "original", "baseline.json"), &baseline); err != nil {
		return Case{}, err
	}
	return Case{Manifest: manifest, Labels: labels, Decision: decision, Baseline: baseline, Dir: dir}, nil
}

func readJSON(path string, dest any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return err
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	return nil
}

func diversifiedKey(c Case) string {
	language, size, _ := caseComposition(c)
	return strings.Join([]string{c.RepoFingerprint, language, size, goldBucket(c.Labels)}, "\x00")
}

func goldBucket(labels Labels) string {
	hasTP, hasFN := false, false
	for _, finding := range labels.Findings {
		switch finding.Kind {
		case GoldTruePositive:
			hasTP = true
		case GoldFalseNegative:
			hasFN = true
		}
	}
	switch {
	case hasTP && hasFN:
		return "true-positive+false-negative"
	case hasTP:
		return GoldTruePositive
	case hasFN:
		return GoldFalseNegative
	default:
		return "unlabeled"
	}
}

func caseComposition(c Case) (language, size, severity string) {
	language = "other"
	if c.ChangedFiles > 1 {
		language = "mixed"
	}
	switch {
	case c.ChangedLines <= 25:
		size = "tiny"
	case c.ChangedLines <= 100:
		size = "small"
	case c.ChangedLines <= 500:
		size = "medium"
	default:
		size = "large"
	}
	severity = "none"
	if raw, err := os.ReadFile(filepath.Join(c.Dir, "original", "round.json")); err == nil {
		var record sourceRound
		if json.Unmarshal(raw, &record) == nil {
			severity = highestSeverity(record.FindingsJSON)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(c.Dir, "original", "changed-files.json")); err == nil {
		var files []string
		if json.Unmarshal(raw, &files) == nil {
			language = dominantLanguage(files, language)
		}
	}
	return language, size, severity
}

func dominantLanguage(files []string, fallback string) string {
	counts := map[string]int{}
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		name := map[string]string{
			".go": "go", ".py": "python", ".js": "javascript", ".jsx": "javascript", ".ts": "typescript", ".tsx": "typescript", ".rs": "rust", ".java": "java", ".rb": "ruby", ".php": "php", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cs": "csharp", ".swift": "swift", ".kt": "kotlin", ".sh": "shell",
		}[ext]
		if name != "" {
			counts[name]++
		}
	}
	if len(counts) == 0 {
		return fallback
	}
	type entry struct {
		name  string
		count int
	}
	entries := make([]entry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, entry{name, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count == entries[j].count {
			return entries[i].name < entries[j].name
		}
		return entries[i].count > entries[j].count
	})
	if len(entries) > 1 && entries[0].count == entries[1].count {
		return "mixed"
	}
	return entries[0].name
}

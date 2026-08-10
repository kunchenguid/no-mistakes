package eval

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
    verdict_known INTEGER NOT NULL,
    verdict_should_park INTEGER NOT NULL,
    path TEXT NOT NULL UNIQUE
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
    expected_park INTEGER,
    candidate_parked INTEGER NOT NULL,
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
	known, shouldPark := 0, 0
	if c.Labels.Verdict.Known {
		known = 1
	}
	if c.Labels.Verdict.ShouldPark {
		shouldPark = 1
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO cases
(id, source_run_id, source_round_id, captured_at, repo_fingerprint, branch, language, size_bucket, severity, verdict_known, verdict_should_park, path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.SourceRunID, c.SourceRoundID, c.CapturedAt, c.RepoFingerprint, c.Branch, language, size, severity, known, shouldPark, c.Dir)
	if err != nil {
		return fmt.Errorf("register eval case: %w", err)
	}
	return nil
}

// ListCases resolves the three MVP logical sets. Diversified is deterministic:
// it retains the lexicographically first case in each repo/language/size/verdict
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
			if c.Labels.Verdict.Known {
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

func loadCase(dir string) (Case, error) {
	var manifest Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &manifest); err != nil {
		return Case{}, err
	}
	if manifest.Version != manifestVersion {
		return Case{}, fmt.Errorf("unsupported case manifest version %d", manifest.Version)
	}
	var labels Labels
	if err := readJSON(filepath.Join(dir, "labels.json"), &labels); err != nil {
		return Case{}, err
	}
	if labels.Version != labelsVersion {
		return Case{}, fmt.Errorf("unsupported case labels version %d", labels.Version)
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
	verdict := "unlabeled"
	if c.Labels.Verdict.Known {
		verdict = "pass"
		if c.Labels.Verdict.ShouldPark {
			verdict = "park"
		}
	}
	return strings.Join([]string{c.RepoFingerprint, language, size, verdict}, "\x00")
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

package firewall

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const storeSchema = `
CREATE TABLE IF NOT EXISTS verdicts (
    id TEXT PRIMARY KEY,
    repo TEXT NOT NULL,
    branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    base_sha TEXT NOT NULL,
    pr_url TEXT,
    pr_number INTEGER,
    status TEXT NOT NULL,
    conclusion TEXT NOT NULL,
    public_summary TEXT NOT NULL,
    portal_url TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS findings (
    id TEXT PRIMARY KEY,
    verdict_id TEXT NOT NULL REFERENCES verdicts(id) ON DELETE CASCADE,
    severity TEXT NOT NULL,
    file TEXT,
    line INTEGER,
    class TEXT NOT NULL,
    action TEXT NOT NULL,
    description TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS responses (
    id TEXT PRIMARY KEY,
    verdict_id TEXT NOT NULL REFERENCES verdicts(id) ON DELETE CASCADE,
    action TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS logs (
    id TEXT PRIMARY KEY,
    verdict_id TEXT NOT NULL REFERENCES verdicts(id) ON DELETE CASCADE,
    line TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
`

// Store holds axi-shaped firewall records. Separate from the pipeline DB.
type Store struct {
	sql *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open firewall store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(storeSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate firewall store: %w", err)
	}
	return &Store{sql: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.sql == nil {
		return nil
	}
	return s.sql.Close()
}

// Verdict is the durable firewall run.
type Verdict struct {
	ID            string     `json:"id"`
	Repo          string     `json:"repo"`
	Branch        string     `json:"branch"`
	HeadSHA       string     `json:"head"`
	BaseSHA       string     `json:"base,omitempty"`
	PRURL         string     `json:"pr"`
	PRNumber      int        `json:"pr_number,omitempty"`
	Status        string     `json:"status"`
	Conclusion    string     `json:"conclusion"`
	PublicSummary string     `json:"public_summary"`
	PortalURL     string     `json:"portal_url"`
	CreatedAt     int64      `json:"created_at"`
	UpdatedAt     int64      `json:"updated_at"`
	Findings      []Finding  `json:"findings,omitempty"`
	Responses     []Response `json:"responses,omitempty"`
}

type Response struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) Insert(in Input, res Result, portalBase string) (*Verdict, error) {
	now := time.Now().Unix()
	id := newFindingID()
	conclusion := res.Conclusion()
	status := "completed"
	if conclusion != "success" {
		status = "failed"
	}
	portalURL := JoinPortalURL(portalBase, id)
	public := PublicText(portalURL, res.Failed())
	_, err := s.sql.Exec(
		`INSERT INTO verdicts (id, repo, branch, head_sha, base_sha, pr_url, pr_number, status, conclusion, public_summary, portal_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Repo, in.Branch, in.HeadSHA, in.BaseSHA, in.PRURL, in.PRNumber,
		status, conclusion, public, portalURL, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert verdict: %w", err)
	}
	for i := range res.Findings {
		f := res.Findings[i]
		if f.ID == "" {
			f.ID = newFindingID()
			res.Findings[i].ID = f.ID
		}
		_, err := s.sql.Exec(
			`INSERT INTO findings (id, verdict_id, severity, file, line, class, action, description, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, id, f.Severity, f.File, f.Line, f.Class, f.Action, f.Description, now,
		)
		if err != nil {
			return nil, fmt.Errorf("insert finding: %w", err)
		}
	}
	logLine := "publish-policy scan complete"
	if res.Error != "" {
		logLine = "publish-policy scan error"
	} else if res.Failed() {
		logLine = PublicPhrase
	}
	if _, err := s.sql.Exec(
		`INSERT INTO logs (id, verdict_id, line, created_at) VALUES (?, ?, ?, ?)`,
		newFindingID(), id, logLine, now,
	); err != nil {
		return nil, fmt.Errorf("insert log: %w", err)
	}
	return s.Get(id)
}

func (s *Store) Get(id string) (*Verdict, error) {
	v := &Verdict{}
	err := s.sql.QueryRow(
		`SELECT id, repo, branch, head_sha, base_sha, pr_url, pr_number, status, conclusion, public_summary, portal_url, created_at, updated_at
		 FROM verdicts WHERE id = ?`, id,
	).Scan(&v.ID, &v.Repo, &v.Branch, &v.HeadSHA, &v.BaseSHA, &v.PRURL, &v.PRNumber,
		&v.Status, &v.Conclusion, &v.PublicSummary, &v.PortalURL, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	rows, err := s.sql.Query(
		`SELECT id, severity, file, line, class, action, description FROM findings WHERE verdict_id = ? ORDER BY created_at`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Finding
		var file sql.NullString
		if err := rows.Scan(&f.ID, &f.Severity, &file, &f.Line, &f.Class, &f.Action, &f.Description); err != nil {
			return nil, err
		}
		f.File = file.String
		v.Findings = append(v.Findings, f)
	}
	resps, err := s.sql.Query(`SELECT id, action, created_at FROM responses WHERE verdict_id = ? ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer resps.Close()
	for resps.Next() {
		var r Response
		if err := resps.Scan(&r.ID, &r.Action, &r.CreatedAt); err != nil {
			return nil, err
		}
		v.Responses = append(v.Responses, r)
	}
	return v, nil
}

func (s *Store) List(limit int) ([]Verdict, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.sql.Query(
		`SELECT id, repo, branch, head_sha, base_sha, pr_url, pr_number, status, conclusion, public_summary, portal_url, created_at, updated_at
		 FROM verdicts ORDER BY created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Verdict
	for rows.Next() {
		var v Verdict
		if err := rows.Scan(&v.ID, &v.Repo, &v.Branch, &v.HeadSHA, &v.BaseSHA, &v.PRURL, &v.PRNumber,
			&v.Status, &v.Conclusion, &v.PublicSummary, &v.PortalURL, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) Logs(id string) ([]string, error) {
	rows, err := s.sql.Query(`SELECT line FROM logs WHERE verdict_id = ? ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func (s *Store) Respond(id, action string) (*Verdict, error) {
	if _, err := s.Get(id); err != nil {
		return nil, err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "acknowledge"
	}
	_, err := s.sql.Exec(
		`INSERT INTO responses (id, verdict_id, action, created_at) VALUES (?, ?, ?, ?)`,
		newFindingID(), id, action, time.Now().Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("insert response: %w", err)
	}
	// Acknowledgement is recorded only. The GitHub check stays failed until
	// a new head SHA is scanned.
	return s.Get(id)
}

// StatusCancelled marks a verdict an operator aborted on the LAN portal. It
// changes the rendered run outcome; it never changes the GitHub conclusion.
const StatusCancelled = "cancelled"

func (s *Store) Abort(id string) (*Verdict, error) {
	if _, err := s.Get(id); err != nil {
		return nil, err
	}
	_, err := s.sql.Exec(`UPDATE verdicts SET status = ?, updated_at = ? WHERE id = ?`, StatusCancelled, time.Now().Unix(), id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// AxiRun is the JSON object that mirrors axi status `run:`.
type AxiRun struct {
	ID      string    `json:"id"`
	Branch  string    `json:"branch"`
	Status  string    `json:"status"`
	Head    string    `json:"head"`
	PR      string    `json:"pr"`
	Outcome string    `json:"outcome,omitempty"`
	Steps   []AxiStep `json:"steps"`
	Gate    *AxiGate  `json:"gate,omitempty"`
	Help    []string  `json:"help,omitempty"`
}

type AxiStep struct {
	Step       string       `json:"step"`
	Status     string       `json:"status"`
	Findings   int          `json:"findings"`
	DurationMS int64        `json:"duration_ms"`
	Items      []AxiFinding `json:"items,omitempty"`
}

type AxiFinding struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	File        string `json:"file"`
	Line        int    `json:"line,omitempty"`
	Action      string `json:"action"`
	Class       string `json:"class,omitempty"`
	Description string `json:"description"`
}

type AxiGate struct {
	Step     string       `json:"step"`
	Status   string       `json:"status"`
	Findings []AxiFinding `json:"findings"`
}

func (v *Verdict) AxiRun() AxiRun {
	items := make([]AxiFinding, 0, len(v.Findings))
	for _, f := range v.Findings {
		items = append(items, AxiFinding{
			ID:          f.ID,
			Severity:    f.Severity,
			File:        f.File,
			Line:        f.Line,
			Action:      f.Action,
			Class:       string(f.Class),
			Description: f.Description,
		})
	}
	stepStatus := "completed"
	switch {
	case v.Status == StatusCancelled:
		stepStatus = StatusCancelled
	case v.Conclusion != "success":
		stepStatus = "failed"
	}
	run := AxiRun{
		ID:     v.ID,
		Branch: v.Branch,
		Status: v.Status,
		Head:   v.HeadSHA,
		PR:     v.PRURL,
		Steps: []AxiStep{{
			Step:     "publish-firewall",
			Status:   stepStatus,
			Findings: len(items),
			Items:    items,
		}},
	}
	switch {
	case v.Status == StatusCancelled:
		run.Outcome = StatusCancelled
		run.Help = []string{
			"Aborted on the LAN portal; no operator decision is pending",
			"The GitHub check keeps its recorded conclusion until a new head SHA is scanned",
		}
	case v.Conclusion == "success":
		run.Outcome = "passed"
	default:
		run.Outcome = "failed"
		run.Gate = &AxiGate{
			Step:     "publish-firewall",
			Status:   "awaiting_approval",
			Findings: items,
		}
		run.Help = []string{
			"Public GitHub output is generic; match details stay on this LAN record",
			"Respond with action acknowledge to record that an operator saw the verdict; the GitHub check stays failed until a new head SHA is scanned",
		}
	}
	return run
}

func (v *Verdict) Notice() Notice {
	return NewNotice(v.Repo, v.PRURL, v.PortalURL, v.Conclusion)
}

package db

import "fmt"

// RunAgentSession is the minimum session-resume metadata for one durable
// per-run, per-role agent session (the review loop's reviewer and fixer
// roles). Only the adapter-native session identity is stored - never prompts,
// transcripts, or any conversation content - so the review loop can resume
// its role sessions across parking and daemon process boundaries.
type RunAgentSession struct {
	RunID     string
	Role      string
	Agent     string
	SessionID string
	CreatedAt int64
	UpdatedAt int64
}

// UpsertRunAgentSession stores or replaces the session identity for a
// run+role. A run has at most one session per role.
func (d *DB) UpsertRunAgentSession(runID, role, agent, sessionID string) error {
	ts := now()
	_, err := d.sql.Exec(
		`INSERT INTO run_agent_sessions (run_id, role, agent, session_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id, role) DO UPDATE SET agent = excluded.agent, session_id = excluded.session_id, updated_at = excluded.updated_at`,
		runID, role, agent, sessionID, ts, ts,
	)
	if err != nil {
		return fmt.Errorf("upsert run agent session: %w", err)
	}
	return nil
}

// GetRunAgentSessions returns all stored session identities for a run.
func (d *DB) GetRunAgentSessions(runID string) ([]RunAgentSession, error) {
	rows, err := d.sql.Query(
		`SELECT run_id, role, agent, session_id, created_at, updated_at FROM run_agent_sessions WHERE run_id = ?`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get run agent sessions: %w", err)
	}
	defer rows.Close()

	var sessions []RunAgentSession
	for rows.Next() {
		var s RunAgentSession
		if err := rows.Scan(&s.RunID, &s.Role, &s.Agent, &s.SessionID, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan run agent session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// DeleteRunAgentSession drops one role's session identity so the next turn
// starts a fresh same-role session (used after a failed resume).
func (d *DB) DeleteRunAgentSession(runID, role string) error {
	_, err := d.sql.Exec(`DELETE FROM run_agent_sessions WHERE run_id = ? AND role = ?`, runID, role)
	if err != nil {
		return fmt.Errorf("delete run agent session: %w", err)
	}
	return nil
}

package db

import (
	"path/filepath"
	"testing"
)

func openSessionTestDB(t *testing.T) (*DB, *Repo, *Run) {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return d, repo, run
}

func TestRunAgentSessions_UpsertGetDelete(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	if err := d.UpsertRunAgentSession(run.ID, "reviewer", "codex", "thread-1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.UpsertRunAgentSession(run.ID, "review-fixer", "codex", "thread-2"); err != nil {
		t.Fatalf("upsert fixer: %v", err)
	}

	sessions, err := d.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	byRole := map[string]RunAgentSession{}
	for _, s := range sessions {
		byRole[s.Role] = s
	}
	if byRole["reviewer"].SessionID != "thread-1" || byRole["review-fixer"].SessionID != "thread-2" {
		t.Fatalf("unexpected sessions: %+v", byRole)
	}

	// Upsert replaces the same role's identity instead of adding a row.
	if err := d.UpsertRunAgentSession(run.ID, "reviewer", "codex", "thread-3"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	sessions, err = d.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatalf("get after re-upsert: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions after re-upsert, want 2", len(sessions))
	}

	if err := d.DeleteRunAgentSession(run.ID, "reviewer"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	sessions, err = d.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Role != "review-fixer" {
		t.Fatalf("delete removed the wrong session: %+v", sessions)
	}
}

// TestRunAgentSessions_IsolatedPerRun verifies two runs never see each
// other's session identities even for the same role and agent.
func TestRunAgentSessions_IsolatedPerRun(t *testing.T) {
	d, repo, run1 := openSessionTestDB(t)
	run2, err := d.InsertRun(repo.ID, "feature/y", "head2", "base2")
	if err != nil {
		t.Fatalf("insert run2: %v", err)
	}

	if err := d.UpsertRunAgentSession(run1.ID, "reviewer", "codex", "thread-run1"); err != nil {
		t.Fatalf("upsert run1: %v", err)
	}
	if err := d.UpsertRunAgentSession(run2.ID, "reviewer", "codex", "thread-run2"); err != nil {
		t.Fatalf("upsert run2: %v", err)
	}

	sessions, err := d.GetRunAgentSessions(run1.ID)
	if err != nil {
		t.Fatalf("get run1: %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "thread-run1" {
		t.Fatalf("run1 sessions leaked or missing: %+v", sessions)
	}
}

// TestOpenMigratesRunAgentSessionsTable proves a database created before the
// run_agent_sessions table existed gains it on reopen.
func TestOpenMigratesRunAgentSessionsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := d.sql.Exec(`DROP TABLE run_agent_sessions`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "b", "h", "b")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.UpsertRunAgentSession(run.ID, "reviewer", "claude", "sess"); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
}

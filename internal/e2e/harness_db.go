//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// OpenDB opens the daemon's SQLite database for direct assertions on durable
// routing history. The caller must Close the returned handle.
func (h *Harness) OpenDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open db at %s: %v", h.NMHome, err)
	}
	return d
}

// InvocationAttempts returns every pipeline invocation attempt for a run in
// durable start order: the routed Candidate (profile, tier, model, effort), the
// terminal outcome, and any circuit-skip failure domain. This is the durable
// source of truth for asserting routing cascades and provider circuits.
func (h *Harness) InvocationAttempts(t *testing.T, runID string) []*db.InvocationAttempt {
	t.Helper()
	d := h.OpenDB(t)
	defer d.Close()
	attempts, err := d.GetInvocationAttemptsByRun(runID)
	if err != nil {
		t.Fatalf("invocation attempts for run %s: %v", runID, err)
	}
	return attempts
}

// FindingRepairs returns a run's finding-repair cycles in creation order: the
// lineage id, tier, remaining budget, verdict, status, and fixer/verifier
// attempt links that record the escalation lineage.
func (h *Harness) FindingRepairs(t *testing.T, runID string) []*db.FindingRepair {
	t.Helper()
	d := h.OpenDB(t)
	defer d.Close()
	repairs, err := d.GetFindingRepairsByRun(runID)
	if err != nil {
		t.Fatalf("finding repairs for run %s: %v", runID, err)
	}
	return repairs
}

// RestartDaemon restarts the daemon in place and verifies it comes back up, so
// a journey can assert that persisted routing history survives a reconnect.
func (h *Harness) RestartDaemon(t *testing.T) {
	t.Helper()
	out, err := h.Run("daemon", "restart")
	if err != nil {
		t.Fatalf("daemon restart: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon restarted") {
		t.Fatalf("daemon restart output = %q, want it to contain 'daemon restarted'", out)
	}
}

// RespondFix drives an ActionFix approval selecting specific finding IDs — the
// consent path that authorizes repairing an ask-user finding through the
// intent-sensitive cascade.
func (h *Harness) RespondFix(t *testing.T, runID string, step types.StepName, findingIDs ...string) {
	t.Helper()
	client, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()
	var result ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: runID, Step: step, Action: types.ActionFix, FindingIDs: findingIDs}, &result); err != nil {
		t.Fatalf("respond fix to run %s: %v", runID, err)
	}
	if !result.OK {
		t.Fatalf("respond fix to run %s returned not OK", runID)
	}
}

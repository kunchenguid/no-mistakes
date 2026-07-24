//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTerminalPRRunDisappearsFromActiveListing reproduces the operator-visible
// defect with a real CLI and an isolated daemon/home. A terminal PR observation
// is the initiating trigger. The normal executor's immediate follow-up status
// write usually masks the defect, so this fixture stops at the durable
// observation boundary where an interruption used to leave status=running.
func TestTerminalPRRunDisappearsFromActiveListing(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}

			p := paths.WithRoot(h.NMHome)
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(h.repoID(), "feature/terminal-pr", "0123456789abcdef", "fedcba9876543210")
			if err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.UpdateRunPRState(run.ID, state); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			out, err := h.Run("runs")
			if err != nil {
				t.Fatalf("runs: %v\n%s", err, out)
			}
			if !strings.Contains(out, "completed") || strings.Contains(out, "running") {
				t.Fatalf("terminal PR remained visibly active:\n%s", out)
			}

			activeDB, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer activeDB.Close()
			active, err := activeDB.GetActiveRuns()
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 0 {
				t.Fatalf("authoritative DB still lists terminal PR run as active: %+v", active)
			}
		})
	}
}

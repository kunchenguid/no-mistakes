//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A terminal PR observation does not certify support or finish a run.
func TestTerminalPRObservationDoesNotCompleteRun(t *testing.T) {
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
			t.Logf("no-mistakes runs after %s PR observation:\n%s", state, out)
			if strings.Contains(out, "completed") || !strings.Contains(out, "running") {
				t.Fatalf("unverified PR observation incorrectly completed the run:\n%s", out)
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
			if len(active) != 1 || active[0].ID != run.ID {
				t.Fatalf("unverified PR observation lost its active run: %+v", active)
			}
		})
	}
}

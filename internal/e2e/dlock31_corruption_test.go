//go:build e2e

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func mutateDLOCK31Repair(t *testing.T, h *Harness, database *db.DB, binding *db.RetainedCIRepair, variant string) string {
	t.Helper()
	workdir := dlock31Workdir(t, database, binding.RunID)
	switch variant {
	case "malformed-binding":
		connection, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		result, err := connection.Exec("UPDATE retained_ci_repairs SET retained_head = 'not-a-sha' WHERE run_id = ?", binding.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			t.Fatalf("corrupted rows=%d %v", count, err)
		}
	case "unbound":
		if err := database.ClearRetainedCIRepair(binding.RunID, binding.RetainedHead); err != nil {
			t.Fatal(err)
		}
		return "Unbound retained CI correction"
	case "dirty":
		writeDLOCK31(t, filepath.Join(workdir, "dirty.txt"), "owned negative control\n")
	case "moved-anchor":
		dlock31Git(t, h, workdir, "update-ref", "refs/no-mistakes/ci-repair/"+binding.RunID, binding.RecordedHead)
	case "divergent", "moved-head":
		if variant == "divergent" {
			dlock31Git(t, h, workdir, "checkout", "--detach", binding.RecordedHead)
		}
		dlock31Git(t, h, workdir, "-c", "user.name=E2E Test", "-c", "user.email=e2e@example.com", "commit", "--allow-empty", "-m", "owned unrelated correction")
	case "policy-changed":
		changeDLOCK31TrustedPolicy(t, h, binding.Branch)
		return "requires Review under the trusted policy"
	default:
		t.Fatalf("unknown corruption %s", variant)
	}
	return "retained CI repair verification failed"
}

func changeDLOCK31TrustedPolicy(t *testing.T, h *Harness, branch string) {
	t.Helper()
	dlock31Git(t, h, h.WorkDir, "checkout", "main")
	configPath := filepath.Join(h.WorkDir, ".no-mistakes.yaml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	writeDLOCK31(t, configPath, string(before)+"\nci:\n  revalidate_repairs: true\n")
	dlock31Git(t, h, h.WorkDir, "add", ".no-mistakes.yaml")
	dlock31Git(t, h, h.WorkDir, "commit", "-m", "require review after CI repair")
	dlock31Git(t, h, h.WorkDir, "push", h.UpstreamDir, "HEAD:refs/heads/main")
	dlock31Git(t, h, h.WorkDir, "checkout", branch)
	previous := daemonPIDForRoot(t, h)
	if out, err := h.Run("daemon", "restart", "--force"); err != nil {
		t.Fatalf("owned policy reload: %v %s", err, out)
	}
	if daemonPIDForRoot(t, h) == previous {
		t.Fatal("policy reload did not restart the owned daemon")
	}
}

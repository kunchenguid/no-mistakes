package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestAxiCanaryReportsDormantThenActivated proves the operator surface reports a
// dormant canary before activation and the baseline/routed comparison after,
// including the advisory pending target state with empty cohorts.
func TestAxiCanaryReportsDormantThenActivated(t *testing.T) {
	repoDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")
	rawRoot, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		rawRoot = repoDir
	}
	chdir(t, rawRoot)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	if _, err := database.InsertRepoWithID("repo-1", rawRoot, "origin", "main"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	dormant := captureCanary(t)
	for _, want := range []string{
		"surface: routing-canary",
		"report_required: true",
		"activated: false",
		"target_advisory: true",
		"comparison_complete: false",
		"result_state: dormant",
		"baseline_complete: false",
		"routed_complete: false",
		"target_met: pending",
	} {
		if !strings.Contains(dormant, want) {
			t.Fatalf("dormant canary missing %q in:\n%s", want, dormant)
		}
	}

	if _, err := database.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	active := captureCanary(t)
	for _, want := range []string{
		"activated: true",
		"report_required: true",
		"target_advisory: true",
		"comparison_complete: false",
		"result_state: preliminary",
		"baseline_complete: false",
		"routed_complete: false",
		"baseline_runs: 0",
		"routed_runs: 0",
		"target_met: pending",
	} {
		if !strings.Contains(active, want) {
			t.Fatalf("activated canary missing %q in:\n%s", want, active)
		}
	}
}

func TestAxiCanaryHelpRequiresReportAndRejectsPreliminaryResults(t *testing.T) {
	help := newAxiCanaryCmd().Long
	for _, want := range []string{"report is required", "preliminary", "must not be treated as live results", "advisory"} {
		if !strings.Contains(help, want) {
			t.Fatalf("canary help missing %q in:\n%s", want, help)
		}
	}
}

func captureCanary(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runAxiCanary(cmd); err != nil {
		t.Fatalf("axi canary: %v\n%s", err, out.String())
	}
	return out.String()
}

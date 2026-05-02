package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/wizard"
)

func TestActiveRunBranchUsesRepoWideLookupForExplicitAttach(t *testing.T) {
	tests := []struct {
		name        string
		rootDefault bool
		want        string
	}{
		{name: "root command stays branch scoped", rootDefault: true, want: "feature/current"},
		{name: "attach subcommand falls back across branches", rootDefault: false, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &repoState{currentBranch: "feature/current"}
			if got := activeRunBranch(state, tc.rootDefault); got != tc.want {
				t.Fatalf("activeRunBranch() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttachRunIDWithUnknownRunReturnsHelpfulError(t *testing.T) {
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	out, err := executeCmd("attach", "--run", "missing-run")
	if err == nil {
		t.Fatal("attach should fail for an unknown run ID")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Fatalf("attach error should mention missing run, got: %v\noutput: %s", err, out)
	}
}

// TestRootInteractiveWizardFailsLoudlyWhenRunRegistrationIsSlow covers
// issue #122 defect 3. Prior behavior: if the daemon didn't register a
// run after push (e.g. gate hook disabled by husky), the wizard's wait
// silently returned nil, the wizard declared success, and the command
// printed "No active run" with exit code 0. That masked the real failure.
// After the fix, an error surfaces all the way to the CLI.
func TestRootInteractiveWizardFailsLoudlyWhenRunRegistrationIsSlow(t *testing.T) {
	setupTestRepo(t)
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}

	startTestDaemon(t, p, d)

	prevInteractive := terminalInteractive
	terminalInteractive = func() bool { return true }
	defer func() { terminalInteractive = prevInteractive }()

	prevWizardRun := wizardRun
	wizardRun = func(cfg wizard.Config) (wizard.Result, error) {
		if cfg.WaitForRun == nil {
			t.Fatal("expected wait function")
		}
		if err := cfg.WaitForRun(context.Background(), "feat/slow"); err != nil {
			return wizard.Result{}, err
		}
		return wizard.Result{Success: true, Pushed: true, TargetBranch: "feat/slow"}, nil
	}
	defer func() { wizardRun = prevWizardRun }()

	prevRunTUI := runTUI
	runTUI = func(string, *ipc.Client, *ipc.RunInfo, string) error {
		t.Fatal("should not attach when no run is visible yet")
		return nil
	}
	defer func() { runTUI = prevRunTUI }()

	_, err = executeCmd()
	if err == nil {
		t.Fatal("executeCmd() should return an error when the daemon doesn't register a run")
	}
	if !strings.Contains(err.Error(), "feat/slow") {
		t.Errorf("error should name the branch we were waiting for, got: %v", err)
	}
}

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

// TestAttachNotInitialized verifies that running bare `no-mistakes` in a git
// repo that hasn't been initialized returns a clear error instead of panicking.
// This is the exact scenario that caused the nil pointer dereference: db.GetRepoByPath
// returns (nil, nil) for unknown repos, and the code dereferenced repo.ID without
// a nil check.
func TestAttachNotInitializedCommands(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Start daemon so attachRun gets past EnsureDaemon + Dial.
	startTestDaemon(t, p, d)

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "root", args: nil},
		{name: "attach", args: []string{"attach"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err = executeCmd(test.args...)
			if err == nil {
				t.Fatalf("%s command in uninitialized repo should return an error", test.name)
			}
			if !strings.Contains(err.Error(), "not initialized") {
				t.Errorf("error should mention 'not initialized', got: %v", err)
			}
		})
	}
}

// TestAttachNotGitRepo verifies that running bare `no-mistakes` outside any git
// repo returns a clear error.
func TestAttachNotGitRepoCommands(t *testing.T) {
	tmpDir := t.TempDir()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	chdir(t, tmpDir)

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "root", args: nil},
		{name: "attach", args: []string{"attach"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err = executeCmd(test.args...)
			if err == nil {
				t.Fatalf("%s command outside git repo should return an error", test.name)
			}
			if !strings.Contains(err.Error(), "not in a git repository") {
				t.Errorf("error should mention 'not in a git repository', got: %v", err)
			}
		})
	}
}

func TestRootInteractiveWizardFallsBackWhenRunRegistrationIsSlow(t *testing.T) {
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

	out, err := executeCmd()
	if err != nil {
		t.Fatalf("executeCmd() error = %v", err)
	}
	if !strings.Contains(out, "No active run") {
		t.Fatalf("expected existing fallback output, got %q", out)
	}
}

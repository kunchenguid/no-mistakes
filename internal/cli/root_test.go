package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/wizard"
)

func TestRootYesRunsWizardNonInteractively(t *testing.T) {
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

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	prevAuto := runWizardAuto
	runWizardAuto = func(ctx context.Context, p *paths.Paths, state *repoState, _ waitForRunFunc) (wizard.Result, error) {
		if state == nil {
			t.Fatal("expected repo state")
		}
		if _, err := d.InsertRun(repo.ID, "feat/auto", "head1234", "base5678"); err != nil {
			return wizard.Result{}, err
		}
		return wizard.Result{Success: true, Pushed: true, TargetBranch: "feat/auto"}, nil
	}
	defer func() { runWizardAuto = prevAuto }()

	prevRunTUI := runTUI
	attached := false
	runTUI = func(string, *ipc.Client, *ipc.RunInfo, string) error {
		attached = true
		return nil
	}
	defer func() { runTUI = prevRunTUI }()

	if _, err := executeCmd("-y"); err != nil {
		t.Fatalf("executeCmd(-y) error = %v", err)
	}
	if !attached {
		t.Fatal("expected -y run to attach to the created run")
	}
}

func TestRootYesUsesVisibleWizardWhenInteractive(t *testing.T) {
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

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	prevInteractive := terminalInteractive
	terminalInteractive = func() bool { return true }
	defer func() { terminalInteractive = prevInteractive }()

	prevVisible := runWizardAutoVisible
	visible := false
	runWizardAutoVisible = func(ctx context.Context, p *paths.Paths, state *repoState, _ waitForRunFunc) (wizard.Result, error) {
		visible = true
		if state == nil {
			t.Fatal("expected repo state")
		}
		if _, err := d.InsertRun(repo.ID, "feat/visible", "head1234", "base5678"); err != nil {
			return wizard.Result{}, err
		}
		return wizard.Result{Success: true, Pushed: true, TargetBranch: "feat/visible"}, nil
	}
	defer func() { runWizardAutoVisible = prevVisible }()

	prevAuto := runWizardAuto
	runWizardAuto = func(context.Context, *paths.Paths, *repoState, waitForRunFunc) (wizard.Result, error) {
		t.Fatal("expected interactive -y path to show the wizard instead of using headless auto mode")
		return wizard.Result{}, nil
	}
	defer func() { runWizardAuto = prevAuto }()

	prevRunTUI := runTUI
	attached := false
	runTUI = func(string, *ipc.Client, *ipc.RunInfo, string) error {
		attached = true
		return nil
	}
	defer func() { runTUI = prevRunTUI }()

	if _, err := executeCmd("-y"); err != nil {
		t.Fatalf("executeCmd(-y) error = %v", err)
	}
	if !visible {
		t.Fatal("expected -y run to launch the visible wizard path")
	}
	if !attached {
		t.Fatal("expected -y run to attach to the created run")
	}
}

func TestRootYesFailsWhenWizardPushProducesNoRun(t *testing.T) {
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

	prevAuto := runWizardAuto
	runWizardAuto = func(ctx context.Context, p *paths.Paths, state *repoState, _ waitForRunFunc) (wizard.Result, error) {
		return wizard.Result{Success: true, Pushed: true, TargetBranch: "feat/missing"}, nil
	}
	defer func() { runWizardAuto = prevAuto }()

	_, err = executeCmd("-y")
	if err == nil {
		t.Fatal("expected -y to fail when no active run appears after push")
	}
	if !strings.Contains(err.Error(), "no active run") {
		t.Fatalf("error should mention missing active run, got %v", err)
	}
}

func TestRootYesPassesCommandContextToWizard(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prevAuto := runWizardAuto
	runWizardAuto = func(got context.Context, p *paths.Paths, state *repoState, _ waitForRunFunc) (wizard.Result, error) {
		if got == nil {
			t.Fatal("expected command context")
		}
		if err := got.Err(); err != context.Canceled {
			t.Fatalf("wizard context err = %v, want %v", err, context.Canceled)
		}
		return wizard.Result{}, got.Err()
	}
	defer func() { runWizardAuto = prevAuto }()

	_, err = executeCmdWithContext(ctx, "-y")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeCmdWithContext(-y) error = %v, want %v", err, context.Canceled)
	}
}

func TestRootYesStopsWaitingForRunWhenContextCanceled(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prevAuto := runWizardAuto
	runWizardAuto = func(got context.Context, p *paths.Paths, state *repoState, _ waitForRunFunc) (wizard.Result, error) {
		cancel()
		return wizard.Result{Success: true, Pushed: true, TargetBranch: "feat/missing"}, nil
	}
	defer func() { runWizardAuto = prevAuto }()

	start := time.Now()
	_, err = executeCmdWithContext(ctx, "-y")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeCmdWithContext(-y) error = %v, want %v", err, context.Canceled)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("executeCmdWithContext(-y) took %v after cancellation, want under %v", elapsed, time.Second)
	}
}

func TestExecuteReturnsExitCodeOnCommandError(t *testing.T) {
	nonGitDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, nonGitDir)

	if code := Execute(); code != 1 {
		t.Fatalf("Execute() = %d, want 1", code)
	}
}

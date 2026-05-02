package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/wizard"
	"github.com/muesli/termenv"
)

func TestRootVersion(t *testing.T) {
	out, err := executeCmd("--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, buildinfo.String()) {
		t.Errorf("version output %q should contain %q", out, buildinfo.String())
	}
}

func TestSetColorProfileForOutputUsesAsciiForNonTTY(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.TrueColor)
	setColorProfileForOutput(new(bytes.Buffer))

	if lipgloss.ColorProfile() != termenv.Ascii {
		t.Fatalf("ColorProfile = %v, want %v", lipgloss.ColorProfile(), termenv.Ascii)
	}
}

func TestRootDefaultsToAttachWithHistory(t *testing.T) {
	setupTestRepo(t)
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	// Open DB and init gate directly (avoids EnsureDaemon timeout from CLI init).
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}

	// Start an in-process daemon.
	startTestDaemon(t, p, d)

	// Look up the repo to insert runs (use FindGitRoot for macOS symlink consistency).
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Insert enough runs to exercise age formatting and the recent-runs cap.
	timestamps := []int64{
		time.Now().Add(-10 * 24 * time.Hour).Unix(),
		time.Now().Add(-4 * 24 * time.Hour).Unix(),
		time.Now().Add(-26 * time.Hour).Unix(),
		time.Now().Add(-2 * time.Hour).Unix(),
		time.Now().Add(-90 * time.Second).Unix(),
		time.Now().Unix(),
	}
	branches := []string{
		"oldest/skipped",
		"feature/cache",
		"feature/login",
		"fix/crash",
		"fix/lint",
		"feature/recent",
	}
	for i, branch := range branches {
		run, err := d.InsertRun(repo.ID, branch, fmt.Sprintf("head%04d", i), "000000")
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := d.UpdateRunStatus(run.ID, "completed"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := d.UpdateRunError(run.ID, "lint failed"); err != nil {
				t.Fatal(err)
			}
		}
		setRunCreatedAt(t, p.DB(), run.ID, timestamps[i])
	}

	out, err := executeCmd()
	if err != nil {
		t.Fatalf("bare command failed: %v\noutput: %s", err, out)
	}

	// Should show recent runs.
	if !strings.Contains(out, "Recent runs") {
		t.Errorf("expected 'Recent runs' header, got: %s", out)
	}
	if !strings.Contains(out, "feature/login") {
		t.Errorf("expected branch 'feature/login' in output, got: %s", out)
	}
	if !strings.Contains(out, "fix/crash") {
		t.Errorf("expected branch 'fix/crash' in output, got: %s", out)
	}
	for _, want := range []string{"just now", "1 min ago", "2 hours ago", "1 day ago", "4 days ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected age %q in output, got: %s", want, out)
		}
	}
	if strings.Contains(out, "oldest/skipped") {
		t.Errorf("oldest run should be omitted once recent-runs limit is hit, got: %s", out)
	}
	if !strings.Contains(out, "(1 more - run 'no-mistakes runs' to see all)") {
		t.Errorf("expected recent-runs overflow hint, got: %s", out)
	}
	// Should still show push instructions.
	if !strings.Contains(out, "git push no-mistakes") {
		t.Errorf("expected push instructions, got: %s", out)
	}
}

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

func setRunCreatedAt(t *testing.T, dbPath, runID string, ts int64) {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(`UPDATE runs SET created_at = ?, updated_at = ? WHERE id = ?`, ts, ts, runID); err != nil {
		t.Fatal(err)
	}
}

func TestRootErrorFromNonGitDir(t *testing.T) {
	// Running bare `no-mistakes` from a non-git directory should return an
	// error with a useful message, not fail silently.
	// No NM_TEST_START_DAEMON needed: attachRun now checks for a git repo
	// before starting the daemon, so we never spawn a process here.
	nonGitDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, nonGitDir)

	_, err := executeCmd()
	if err == nil {
		t.Fatal("expected error when running from non-git directory, got nil")
	}
	if !strings.Contains(err.Error(), "git repository") {
		t.Errorf("error should mention git repository, got: %v", err)
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

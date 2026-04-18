package cli

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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

func TestRootHelpListsSubcommandsWithoutTriggeringAttach(t *testing.T) {
	out, err := executeCmd("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"init", "eject", "attach", "rerun", "status", "runs", "doctor", "daemon", "update"} {
		if !strings.Contains(out, sub) {
			t.Errorf("help output should list %q command, got: %s", sub, out)
		}
	}
	if strings.Contains(out, "No active run") {
		t.Errorf("help output should not trigger attach fallback, got: %s", out)
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

func TestRootDefaultsToAttachWithAndWithoutHistory(t *testing.T) {
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

	// Run bare `no-mistakes` (no subcommand) - should default to attach behavior.
	out, err := executeCmd()
	if err != nil {
		t.Fatalf("bare command failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "No active run") {
		t.Errorf("expected 'No active run' output, got: %s", out)
	}
	if !strings.Contains(out, "git push no-mistakes") {
		t.Errorf("expected push instructions, got: %s", out)
	}
	if strings.Contains(out, "Recent runs") {
		t.Errorf("did not expect recent runs before history exists, got: %s", out)
	}

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

	out, err = executeCmd()
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

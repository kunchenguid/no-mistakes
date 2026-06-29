package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// setupRepoWithRun creates an initialized repo with a single running run and an
// optional review log, then chdirs into it. It returns the repo path and run ID.
// A single run means resolveRun picks it regardless of the current branch, so
// the shortcut and its axi counterpart always inspect the same run.
func setupRepoWithRun(t *testing.T, withLog bool) (string, string) {
	t.Helper()
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
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepoWithID("repo-1", rawRoot, "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	r, err := database.InsertRun(repo.ID, "feature/x", "head-x", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run running: %v", err)
	}
	if _, err := database.InsertStepResult(r.ID, types.StepReview); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if withLog {
		logDir := p.RunLogDir(r.ID)
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			t.Fatalf("make log dir: %v", err)
		}
		var b strings.Builder
		for i := 0; i < 100; i++ {
			b.WriteString("review log line\n")
		}
		if err := os.WriteFile(filepath.Join(logDir, "review.log"), []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write log: %v", err)
		}
	}
	return rawRoot, r.ID
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	t.Fatalf("error is not an *exitError: %v", err)
	return -1
}

func TestStShortcutMatchesAxiStatus(t *testing.T) {
	_, runID := setupRepoWithRun(t, false)

	cases := []struct {
		name    string
		stArgs  []string
		axiArgs []string
	}{
		{"active run", []string{"st"}, []string{"axi", "status"}},
		{"specific run", []string{"st", "--run", runID}, []string{"axi", "status", "--run", runID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stOut, stErr := executeCmd(tc.stArgs...)
			axiOut, axiErr := executeCmd(tc.axiArgs...)
			if exitCodeOf(t, stErr) != exitCodeOf(t, axiErr) {
				t.Fatalf("exit codes differ: st=%v axi=%v", stErr, axiErr)
			}
			if stOut != axiOut {
				t.Fatalf("st output != axi status output\nst:\n%s\naxi:\n%s", stOut, axiOut)
			}
			if !strings.Contains(stOut, "run:") {
				t.Fatalf("expected run object in st output, got:\n%s", stOut)
			}
		})
	}
}

func TestStShortcutMatchesAxiStatusNoRuns(t *testing.T) {
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

	stOut, stErr := executeCmd("st")
	axiOut, axiErr := executeCmd("axi", "status")
	if exitCodeOf(t, stErr) != 0 || exitCodeOf(t, axiErr) != 0 {
		t.Fatalf("expected exit 0, got st=%v axi=%v", stErr, axiErr)
	}
	if stOut != axiOut {
		t.Fatalf("st output != axi status output\nst:\n%s\naxi:\n%s", stOut, axiOut)
	}
	if !strings.Contains(stOut, "0 runs yet") {
		t.Fatalf("expected no-runs document, got:\n%s", stOut)
	}
}

func TestLgShortcutMatchesAxiLogs(t *testing.T) {
	setupRepoWithRun(t, true)

	cases := []struct {
		name    string
		lgArgs  []string
		axiArgs []string
	}{
		{"tail via lg", []string{"lg", "--step", "review"}, []string{"axi", "logs", "--step", "review"}},
		{"full via logs alias", []string{"logs", "--step", "review", "--full"}, []string{"axi", "logs", "--step", "review", "--full"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lgOut, lgErr := executeCmd(tc.lgArgs...)
			axiOut, axiErr := executeCmd(tc.axiArgs...)
			if exitCodeOf(t, lgErr) != exitCodeOf(t, axiErr) {
				t.Fatalf("exit codes differ: lg=%v axi=%v", lgErr, axiErr)
			}
			if lgOut != axiOut {
				t.Fatalf("lg output != axi logs output\nlg:\n%s\naxi:\n%s", lgOut, axiOut)
			}
			if !strings.Contains(lgOut, "review log line") {
				t.Fatalf("expected log content, got:\n%s", lgOut)
			}
		})
	}
}

func TestLgShortcutMissingStepExitsTwo(t *testing.T) {
	setupRepoWithRun(t, true)

	for _, args := range [][]string{{"lg"}, {"logs"}} {
		out, err := executeCmd(args...)
		if got := exitCodeOf(t, err); got != 2 {
			t.Fatalf("%v exit code = %d, want 2", args, got)
		}
		if !strings.Contains(out, "--step is required") {
			t.Fatalf("%v missing required-step guidance, got:\n%s", args, out)
		}
	}
}

func TestShortcutsRecordOwnTelemetrySurface(t *testing.T) {
	setupRepoWithRun(t, true)

	cases := []struct {
		args       []string
		path       string
		command    string
		notSurface string
	}{
		{[]string{"st"}, "/st", "st", "axi-status"},
		{[]string{"lg", "--step", "review"}, "/lg", "lg", "axi-logs"},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			recorder := &telemetryRecorder{}
			restore := telemetry.SetDefaultForTesting(recorder)
			defer restore()

			_, _ = executeCmd(tc.args...)

			if recorder.find("pageview", "path", tc.path) == nil {
				t.Fatalf("expected %s pageview", tc.path)
			}
			if recorder.find("command", "command", tc.command) == nil {
				t.Fatalf("expected %s command event", tc.command)
			}
			if recorder.find("command", "command", tc.notSurface) != nil {
				t.Fatalf("%s must not record the %s surface", tc.command, tc.notSurface)
			}
		})
	}
}

func TestStatusCommandUnchangedByShortcuts(t *testing.T) {
	setupRepoWithRun(t, false)

	statusOut, _ := executeCmd("status")
	stOut, _ := executeCmd("st")

	for _, want := range []string{"remote:", "daemon:"} {
		if !strings.Contains(statusOut, want) {
			t.Fatalf("human status missing %q, got:\n%s", want, statusOut)
		}
	}
	if strings.Contains(statusOut, "0 runs yet") {
		t.Fatalf("human status should not emit the TOON no-runs document, got:\n%s", statusOut)
	}
	if statusOut == stOut {
		t.Fatalf("human status and st shortcut must be distinct surfaces; both produced:\n%s", statusOut)
	}
}

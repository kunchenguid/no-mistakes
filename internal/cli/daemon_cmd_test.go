package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestDaemonStatusAndStopWhenNotRunning(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("daemon", "status")
	if err != nil {
		t.Fatalf("daemon status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon not running") {
		t.Errorf("expected 'daemon not running', got: %s", out)
	}

	out, err = executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop should succeed when daemon is not running: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Errorf("expected 'daemon stopped', got: %s", out)
	}
}

func TestDaemonStatusAndStopRunning(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	out, err := executeCmd("daemon", "status")
	if err != nil {
		t.Fatalf("daemon status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon running") {
		t.Errorf("expected 'daemon running', got: %s", out)
	}

	out, err = executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Errorf("expected 'daemon stopped', got: %s", out)
	}

	// Verify daemon is actually stopped.
	alive, _ := daemon.IsRunning(p)
	if alive {
		t.Error("daemon should not be running after stop")
	}
}

func TestDaemonRunUsesProvidedRoot(t *testing.T) {
	wantRoot := filepath.Join(t.TempDir(), "nm-home")
	t.Setenv("NM_HOME", "")

	oldRun := daemonRun
	defer func() { daemonRun = oldRun }()

	var gotRoot string
	daemonRun = func() error {
		gotRoot = os.Getenv("NM_HOME")
		return nil
	}

	cmd := newDaemonCmd()
	cmd.SetArgs([]string{"run", "--root", wantRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("daemon run should set NM_HOME to %q, got %q", wantRoot, gotRoot)
	}
}

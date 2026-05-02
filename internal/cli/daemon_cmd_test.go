package cli

import (
	"os"
	"path/filepath"
	"testing"
)

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

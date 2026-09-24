package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithRoot(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.Root(); got != root {
		t.Errorf("Root() = %q, want %q", got, root)
	}
	if got := p.DB(); got != filepath.Join(root, "state.sqlite") {
		t.Errorf("DB() = %q, want %q", got, filepath.Join(root, "state.sqlite"))
	}
	if got := p.Socket(); got != filepath.Join(root, "socket") {
		t.Errorf("Socket() = %q, want %q", got, filepath.Join(root, "socket"))
	}
	if got := p.PIDFile(); got != filepath.Join(root, "daemon.pid") {
		t.Errorf("PIDFile() = %q, want %q", got, filepath.Join(root, "daemon.pid"))
	}
	if got := p.ConfigFile(); got != filepath.Join(root, "config.yaml") {
		t.Errorf("ConfigFile() = %q, want %q", got, filepath.Join(root, "config.yaml"))
	}
}

func TestRepoPaths(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.ReposDir(); got != filepath.Join(root, "repos") {
		t.Errorf("ReposDir() = %q", got)
	}
	if got := p.RepoDir("abc123"); got != filepath.Join(root, "repos", "abc123.git") {
		t.Errorf("RepoDir() = %q", got)
	}
}

func TestWorktreePaths(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.WorktreesDir(); got != filepath.Join(root, "worktrees") {
		t.Errorf("WorktreesDir() = %q", got)
	}
	if got := p.WorktreeDir("repo1", "run1"); got != filepath.Join(root, "worktrees", "repo1", "run1") {
		t.Errorf("WorktreeDir() = %q", got)
	}
}

func TestLogPaths(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.LogsDir(); got != filepath.Join(root, "logs") {
		t.Errorf("LogsDir() = %q", got)
	}
	if got := p.RunLogDir("run1"); got != filepath.Join(root, "logs", "run1") {
		t.Errorf("RunLogDir() = %q", got)
	}
	if got := p.DaemonLog(); got != filepath.Join(root, "logs", "daemon.log") {
		t.Errorf("DaemonLog() = %q", got)
	}
	if got := p.DaemonBootstrapLog(); got != filepath.Join(root, "logs", "daemon-bootstrap.log") {
		t.Errorf("DaemonBootstrapLog() = %q", got)
	}
	if got := p.ManagedServerLog(); got != filepath.Join(root, "logs", "managed-server.log") {
		t.Errorf("ManagedServerLog() = %q", got)
	}
}

func TestNewWithEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NM_HOME", dir)

	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if p.Root() != dir {
		t.Errorf("Root() = %q, want %q", p.Root(), dir)
	}
}

func TestNewRejectsDefaultRootInTests(t *testing.T) {
	t.Setenv("NM_HOME", "")
	t.Setenv("NO_MISTAKES_ALLOW_DEFAULT_ROOT_IN_TESTS", "")

	_, err := New()
	if err == nil {
		t.Fatal("New() should reject the default root under go test")
	}
}

func TestNewDefault(t *testing.T) {
	t.Setenv("NM_HOME", "")
	t.Setenv("NO_MISTAKES_ALLOW_DEFAULT_ROOT_IN_TESTS", "1")

	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".no-mistakes")
	if p.Root() != want {
		t.Errorf("Root() = %q, want %q", p.Root(), want)
	}
}

func TestEnsureDirs(t *testing.T) {
	dir := t.TempDir()
	p := WithRoot(filepath.Join(dir, "nm"))

	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{p.Root(), p.ReposDir(), p.WorktreesDir(), p.LogsDir(), p.ServerPIDsDir()} {
		info, err := os.Stat(d)
		if err != nil {
			t.Errorf("expected dir %q to exist: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %q to be a directory", d)
		}
	}
}

// TestForGate covers the routing contract the managed receive hooks depend on:
// the daemon root that owns a gate is derivable from the gate's own path, so a
// hook helper never has to trust an NM_HOME that git did not set.
func TestForGate(t *testing.T) {
	tests := []struct {
		name string
		gate string
		want string
	}{
		{name: "ordinary root", gate: filepath.Join("/srv", "nm", "repos", "abc123.git"), want: filepath.Join("/srv", "nm")},
		{name: "default-shaped root", gate: filepath.Join("/home", "u", ".no-mistakes", "repos", "abc123.git"), want: filepath.Join("/home", "u", ".no-mistakes")},
		// A gate directly under the filesystem root still has an owning root
		// ("/"), which a suffix-trimming derivation would flatten to empty and
		// then reject, refusing every push to that home.
		{name: "filesystem root as home", gate: filepath.FromSlash("/repos/abc123.git"), want: string(filepath.Separator)},
		{name: "trailing separator", gate: filepath.Join("/srv", "nm", "repos", "abc123.git") + string(filepath.Separator), want: filepath.Join("/srv", "nm")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ForGate(tt.gate)
			if err != nil {
				t.Fatalf("ForGate(%q) error = %v", tt.gate, err)
			}
			if p.Root() != tt.want {
				t.Fatalf("ForGate(%q).Root() = %q, want %q", tt.gate, p.Root(), tt.want)
			}
			if got := p.RepoDir("abc123"); filepath.Clean(got) != filepath.Clean(filepath.Join(tt.want, "repos", "abc123.git")) {
				t.Fatalf("round trip RepoDir = %q", got)
			}
		})
	}
}

// TestForGateRefusesPathsThatAreNotManagedGates: the helper must fail rather
// than fall back to the default root, so a caller holding an unexpected path
// refuses the push instead of routing it to a daemon that does not own it.
func TestForGateRefusesPathsThatAreNotManagedGates(t *testing.T) {
	for _, gate := range []string{
		filepath.Join("/srv", "nm", "repos", "abc123"),
		filepath.Join("/srv", "nm", "worktrees", "abc123.git"),
		filepath.Join("/srv", "abc123.git"),
		"",
	} {
		if p, err := ForGate(gate); err == nil {
			t.Fatalf("ForGate(%q) = %q, want an error", gate, p.Root())
		} else if !strings.Contains(err.Error(), "cannot derive the gate home") {
			t.Fatalf("ForGate(%q) error = %v, want it to name the cause", gate, err)
		}
	}
}

// TestForGateIgnoresAmbientNMHome is the regression proper: the hook helpers
// resolve their root from the gate, so an NM_HOME naming a different root - the
// state a push from an ordinary shell used to produce - cannot retarget them.
func TestForGateIgnoresAmbientNMHome(t *testing.T) {
	t.Setenv("NM_HOME", filepath.Join("/some", "other", "root"))
	gate := filepath.Join("/srv", "nm", "repos", "abc123.git")
	p, err := ForGate(gate)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/srv", "nm"); p.Root() != want {
		t.Fatalf("ForGate root = %q, want %q - an exported NM_HOME must not choose the owning daemon", p.Root(), want)
	}
}

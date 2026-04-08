package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithRoot(t *testing.T) {
	p := WithRoot("/tmp/nm-test")

	if got := p.Root(); got != "/tmp/nm-test" {
		t.Errorf("Root() = %q, want %q", got, "/tmp/nm-test")
	}
	if got := p.DB(); got != "/tmp/nm-test/state.sqlite" {
		t.Errorf("DB() = %q, want %q", got, "/tmp/nm-test/state.sqlite")
	}
	if got := p.Socket(); got != "/tmp/nm-test/socket" {
		t.Errorf("Socket() = %q, want %q", got, "/tmp/nm-test/socket")
	}
	if got := p.PIDFile(); got != "/tmp/nm-test/daemon.pid" {
		t.Errorf("PIDFile() = %q, want %q", got, "/tmp/nm-test/daemon.pid")
	}
	if got := p.ConfigFile(); got != "/tmp/nm-test/config.yaml" {
		t.Errorf("ConfigFile() = %q, want %q", got, "/tmp/nm-test/config.yaml")
	}
}

func TestRepoPaths(t *testing.T) {
	p := WithRoot("/tmp/nm-test")

	if got := p.ReposDir(); got != "/tmp/nm-test/repos" {
		t.Errorf("ReposDir() = %q", got)
	}
	if got := p.RepoDir("abc123"); got != "/tmp/nm-test/repos/abc123.git" {
		t.Errorf("RepoDir() = %q", got)
	}
}

func TestWorktreePaths(t *testing.T) {
	p := WithRoot("/tmp/nm-test")

	if got := p.WorktreesDir(); got != "/tmp/nm-test/worktrees" {
		t.Errorf("WorktreesDir() = %q", got)
	}
	if got := p.WorktreeDir("repo1", "run1"); got != "/tmp/nm-test/worktrees/repo1/run1" {
		t.Errorf("WorktreeDir() = %q", got)
	}
}

func TestLogPaths(t *testing.T) {
	p := WithRoot("/tmp/nm-test")

	if got := p.LogsDir(); got != "/tmp/nm-test/logs" {
		t.Errorf("LogsDir() = %q", got)
	}
	if got := p.RunLogDir("run1"); got != "/tmp/nm-test/logs/run1" {
		t.Errorf("RunLogDir() = %q", got)
	}
	if got := p.DaemonLog(); got != "/tmp/nm-test/logs/daemon.log" {
		t.Errorf("DaemonLog() = %q", got)
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

func TestNewDefault(t *testing.T) {
	t.Setenv("NM_HOME", "")

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

	for _, d := range []string{p.Root(), p.ReposDir(), p.WorktreesDir(), p.LogsDir()} {
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

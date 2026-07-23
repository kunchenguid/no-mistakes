package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/logstore"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestBootstrapCaptureBoundsDirectProcessOutput(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DaemonBootstrapLog(), []byte("previous crash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(p.DaemonBootstrapLog(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		"NM_HOME="+root,
		"NM_DAEMON_HELPER_PROCESS=capture-output",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("capture helper: %v\n%s", err, output)
	}

	policy := logstore.BootstrapPolicy()
	wantSizes := []int64{17, policy.MaxBytes, policy.MaxBytes}
	for i := 0; i <= policy.Backups; i++ {
		path := p.DaemonBootstrapLog()
		if i > 0 {
			path += "." + string(rune('0'+i))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(path), err)
		}
		if info.Size() > policy.MaxBytes {
			t.Errorf("%s size = %d, max = %d", filepath.Base(path), info.Size(), policy.MaxBytes)
		}
		if info.Size() != wantSizes[i] {
			t.Errorf("%s size = %d, want %d", filepath.Base(path), info.Size(), wantSizes[i])
		}
	}
}

func TestRunRejectsCompetingDaemonBeforeBootstrapCapture(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const existing = "active daemon output\n"
	if err := os.WriteFile(p.DaemonBootstrapLog(), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if err := Run(); !errors.Is(err, ErrSingletonLockHeld) {
		t.Fatalf("Run error = %v, want ErrSingletonLockHeld", err)
	}
	got, err := os.ReadFile(p.DaemonBootstrapLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Fatalf("bootstrap log = %q, want %q", got, existing)
	}
	if _, err := os.Stat(p.DaemonBootstrapLog() + ".1"); !os.IsNotExist(err) {
		t.Fatalf("competing daemon created bootstrap backup: %v", err)
	}
}

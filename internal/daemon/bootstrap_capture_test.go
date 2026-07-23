package daemon

import (
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

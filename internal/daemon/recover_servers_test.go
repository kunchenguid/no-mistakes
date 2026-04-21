package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func writePIDRecord(t *testing.T, dir, name string, info agent.ServerPIDInfo) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReapOrphanedServers_NonexistentDirNoop(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	// Don't call EnsureDirs - ServerPIDsDir won't exist.
	reapOrphanedServers(p) // must not panic
}

func TestReapOrphanedServers_EmptyDirNoop(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	reapOrphanedServers(p)
}

func TestReapOrphanedServers_RemovesMalformedFile(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(p.ServerPIDsDir(), "garbage.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	reapOrphanedServers(p)
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("malformed file should be removed, got err=%v", err)
	}
}

func TestReapOrphanedServers_RemovesStaleFileForDeadPID(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-999999.json", agent.ServerPIDInfo{
		PID:       999999, // conventional "almost certainly unused" PID
		Agent:     "opencode",
		Bin:       "/bin/fake",
		StartedAt: time.Now().UTC(),
	})
	reapOrphanedServers(p)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale file should be removed, got err=%v", err)
	}
}

func TestReapOrphanedServers_SkipsAndRemovesOwnPID(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-self.json", agent.ServerPIDInfo{
		PID:       os.Getpid(),
		Agent:     "opencode",
		StartedAt: time.Now().UTC(),
	})
	reapOrphanedServers(p)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("own-pid file should be cleared, got err=%v", err)
	}
}

func TestOtherDaemonAlive_FalseForMissingFile(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if otherDaemonAlive(p) {
		t.Error("expected false when no daemon pid file")
	}
}

func TestOtherDaemonAlive_FalseForOwnPID(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if otherDaemonAlive(p) {
		t.Error("own pid should not count as another daemon")
	}
}

func TestOtherDaemonAlive_FalseForDeadPID(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if otherDaemonAlive(p) {
		t.Error("dead pid should not count as another daemon")
	}
}

func TestOtherDaemonAlive_TrueWhenLivenessCheckErrors(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := processRunningFunc
	processRunningFunc = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return false, fmt.Errorf("transient failure")
	}
	t.Cleanup(func() { processRunningFunc = old })

	if !otherDaemonAlive(p) {
		t.Error("liveness-check errors should conservatively block orphan reaping")
	}
}

func TestOtherDaemonAlive_TrueWhenPIDFileUnreadable(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.PIDFile(), 0o755); err != nil {
		t.Fatal(err)
	}

	if !otherDaemonAlive(p) {
		t.Error("pid-file read errors should conservatively block orphan reaping")
	}
}

func TestOtherDaemonAlive_TrueWhenPIDFileCorrupt(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !otherDaemonAlive(p) {
		t.Error("corrupt pid file should conservatively block orphan reaping")
	}
}

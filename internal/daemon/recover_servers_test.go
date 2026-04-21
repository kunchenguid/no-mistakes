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

func TestReapOrphanedServers_SkipsWizardOwnedRecord(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	const ownerPID = 54321
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-wizard.json", agent.ServerPIDInfo{
		PID:            12345,
		Owner:          agent.ServerPIDOwnerWizard,
		OwnerPID:       ownerPID,
		OwnerStartedAt: startedAt,
		Agent:          "opencode",
		StartedAt:      startedAt,
	})

	oldRunning := processRunningFunc
	oldStartTime := processStartTimeFunc
	oldTerminate := terminateOrphanProcessGroupFunc
	processRunningFunc = func(pid int) (bool, error) {
		if pid != ownerPID {
			t.Fatalf("unexpected pid %d", pid)
		}
		return true, nil
	}
	processStartTimeFunc = func(pid int) (time.Time, error) {
		if pid != ownerPID {
			t.Fatalf("unexpected pid %d", pid)
		}
		return startedAt.Add(time.Second), nil
	}
	terminateOrphanProcessGroupFunc = func(pid int) error {
		t.Fatalf("wizard-owned pid %d should not be terminated", pid)
		return nil
	}
	t.Cleanup(func() {
		processRunningFunc = oldRunning
		processStartTimeFunc = oldStartTime
		terminateOrphanProcessGroupFunc = oldTerminate
	})

	reapOrphanedServers(p)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("wizard-owned pid file should be kept, got err=%v", err)
	}
}

func TestReapOrphanedServers_ReapsWizardOwnedRecordWhenOwnerPIDReused(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-wizard-reused.json", agent.ServerPIDInfo{
		PID:            12345,
		Owner:          agent.ServerPIDOwnerWizard,
		OwnerPID:       54321,
		OwnerStartedAt: startedAt,
		Agent:          "opencode",
		StartedAt:      startedAt,
	})

	oldRunning := processRunningFunc
	oldStartTime := processStartTimeFunc
	oldTerminate := terminateOrphanProcessGroupFunc
	processRunningFunc = func(pid int) (bool, error) {
		switch pid {
		case 54321, 12345:
			return true, nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return false, nil
		}
	}
	processStartTimeFunc = func(pid int) (time.Time, error) {
		switch pid {
		case 54321:
			return startedAt.Add(time.Hour), nil
		case 12345:
			return startedAt, nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return time.Time{}, nil
		}
	}
	terminated := 0
	terminateOrphanProcessGroupFunc = func(pid int) error {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		terminated++
		return nil
	}
	t.Cleanup(func() {
		processRunningFunc = oldRunning
		processStartTimeFunc = oldStartTime
		terminateOrphanProcessGroupFunc = oldTerminate
	})

	reapOrphanedServers(p)

	if terminated != 1 {
		t.Fatalf("expected one terminate call, got %d", terminated)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("wizard-owned pid file should be removed after reap, got err=%v", err)
	}
}

func TestShouldSkipOrphanRecord_FalseWhenWizardOwnerPIDMatchesCurrentDaemonButStartTimeDiffers(t *testing.T) {
	startedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)

	oldRunning := processRunningFunc
	oldStartTime := processStartTimeFunc
	processRunningFunc = func(pid int) (bool, error) {
		if pid != os.Getpid() {
			t.Fatalf("unexpected pid %d", pid)
		}
		return true, nil
	}
	processStartTimeFunc = func(pid int) (time.Time, error) {
		if pid != os.Getpid() {
			t.Fatalf("unexpected pid %d", pid)
		}
		return startedAt.Add(10 * time.Minute), nil
	}
	t.Cleanup(func() {
		processRunningFunc = oldRunning
		processStartTimeFunc = oldStartTime
	})

	if shouldSkipOrphanRecord(agent.ServerPIDInfo{
		Owner:          agent.ServerPIDOwnerWizard,
		OwnerPID:       os.Getpid(),
		OwnerStartedAt: startedAt,
	}) {
		t.Fatal("expected mismatched current-daemon pid reuse to be reaped")
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

func TestOtherDaemonAlive_FalseWhenPIDReusedByNewerProcess(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordedStart := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	pidData, err := json.Marshal(daemonPIDFile{PID: 12345, StartedAt: recordedStart})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), pidData, 0o644); err != nil {
		t.Fatal(err)
	}

	oldRunning := processRunningFunc
	oldStartTime := processStartTimeFunc
	processRunningFunc = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return true, nil
	}
	processStartTimeFunc = func(pid int) (time.Time, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return recordedStart.Add(time.Hour), nil
	}
	t.Cleanup(func() {
		processRunningFunc = oldRunning
		processStartTimeFunc = oldStartTime
	})

	if otherDaemonAlive(p) {
		t.Error("reused pid from a newer process should not block orphan reaping")
	}
}

func TestOtherDaemonAlive_FalseWhenLegacyPIDFileMatchesReusedPID(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p.PIDFile(), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	oldRunning := processRunningFunc
	oldStartTime := processStartTimeFunc
	processRunningFunc = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return true, nil
	}
	processStartTimeFunc = func(pid int) (time.Time, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return mtime.Add(time.Hour), nil
	}
	t.Cleanup(func() {
		processRunningFunc = oldRunning
		processStartTimeFunc = oldStartTime
	})

	if otherDaemonAlive(p) {
		t.Error("legacy pid file with reused pid should not block orphan reaping")
	}
}

func TestOtherDaemonAlive_TrueWhenDaemonStartTimeMatchesRecord(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	recordedStart := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	pidData, err := json.Marshal(daemonPIDFile{PID: 12345, StartedAt: recordedStart})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), pidData, 0o644); err != nil {
		t.Fatal(err)
	}

	oldRunning := processRunningFunc
	oldStartTime := processStartTimeFunc
	processRunningFunc = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return true, nil
	}
	processStartTimeFunc = func(pid int) (time.Time, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return recordedStart.Add(time.Second), nil
	}
	t.Cleanup(func() {
		processRunningFunc = oldRunning
		processStartTimeFunc = oldStartTime
	})

	if !otherDaemonAlive(p) {
		t.Error("matching daemon start time should block orphan reaping")
	}
}

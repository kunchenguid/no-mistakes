package logstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDaemonLogPolicies(t *testing.T) {
	tests := []struct {
		name string
		got  Policy
		want Policy
	}{
		{name: "lifecycle", got: LifecyclePolicy(), want: Policy{MaxBytes: 32 << 20, Backups: 3}},
		{name: "managed server", got: ManagedServerPolicy(), want: Policy{MaxBytes: 16 << 20, Backups: 2}},
		{name: "bootstrap", got: BootstrapPolicy(), want: Policy{MaxBytes: 1 << 20, Backups: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("policy = %+v, want %+v", tt.got, tt.want)
			}
		})
	}
}

func TestRotatingWriterBoundsBytesAndRetainsNewestBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	policy := Policy{MaxBytes: 4, Backups: 2}
	w, err := Open(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if n, err := w.Write([]byte("AAAABBBBCCCCDD")); err != nil || n != 14 {
		t.Fatalf("Write = %d, %v, want 14, nil", n, err)
	}

	assertFileContent(t, path, "DD")
	assertFileContent(t, path+".1", "CCCC")
	assertFileContent(t, path+".2", "BBBB")
	assertNoBackup(t, path+".3")
	assertBoundedFiles(t, path, policy)
}

func TestRotatingWriterConcurrentWritesStayBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed-server.log")
	policy := Policy{MaxBytes: 128, Backups: 3}
	w, err := Open(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	const writers = 12
	const writesPerGoroutine = 200
	line := bytes.Repeat([]byte("x"), 17)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writesPerGoroutine; j++ {
				if _, err := w.Write(line); err != nil {
					t.Errorf("concurrent Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	assertBoundedFiles(t, path, policy)
	for i := 1; i <= policy.Backups; i++ {
		if _, err := os.Stat(fmt.Sprintf("%s.%d", path, i)); err != nil {
			t.Fatalf("expected retained backup %d: %v", i, err)
		}
	}
	assertNoBackup(t, fmt.Sprintf("%s.%d", path, policy.Backups+1))
}

func TestRotationKeepsCurrentInodeForHeldDescriptors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	policy := Policy{MaxBytes: 4, Backups: 1}
	w, err := Open(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	held, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if _, err := w.Write([]byte("old!")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("n")); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Write([]byte("ew")); err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, path+".1", "old!")
	assertFileContent(t, path, "new")
	assertBoundedFiles(t, path, policy)
}

func TestOpenCompactsLegacyUnboundedLogAndPrunesRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("00001111222233334444"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".9", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := Policy{MaxBytes: 4, Backups: 2}
	w, err := Open(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	assertFileContent(t, path, "4444")
	assertFileContent(t, path+".1", "3333")
	assertFileContent(t, path+".2", "2222")
	assertNoBackup(t, path+".9")
	assertBoundedFiles(t, path, policy)
}

func TestRotateAtStartupPreservesCrashOutputAndHeldDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap.log")
	if err := os.WriteFile(path, []byte("previous crash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	policy := Policy{MaxBytes: 32, Backups: 2}

	if err := RotateAtStartup(path, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := held.WriteString("current bootstrap\n"); err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, path+".1", "previous crash\n")
	assertFileContent(t, path, "current bootstrap\n")
	assertBoundedFiles(t, path, policy)
}

func assertBoundedFiles(t *testing.T, path string, policy Policy) {
	t.Helper()
	for i := 0; i <= policy.Backups; i++ {
		candidate := path
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", path, i)
		}
		info, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > policy.MaxBytes {
			t.Errorf("%s size = %d, max = %d", candidate, info.Size(), policy.MaxBytes)
		}
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func assertNoBackup(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unexpected backup %s, stat error = %v", path, err)
	}
}

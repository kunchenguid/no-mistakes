//go:build !windows

package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseProcessStartTimeUsesProvidedLocation(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*60*60)

	startedAt, err := parseProcessStartTime("Mon Jan 2 15:04:05 2006", loc)
	if err != nil {
		t.Fatalf("parseProcessStartTime returned error: %v", err)
	}

	if startedAt.Location() != loc {
		t.Fatalf("location = %v, want %v", startedAt.Location(), loc)
	}
	if startedAt.UTC() != time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC) {
		t.Fatalf("utc time = %v, want %v", startedAt.UTC(), time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC))
	}
}

func TestProcessStartTimeCommandForcesCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")

	cmd := processStartTimeCommand(123)

	if got := strings.Join(cmd.Args[1:], " "); got != "-p 123 -o lstart=" {
		t.Fatalf("args = %q, want %q", got, "-p 123 -o lstart=")
	}
	if filepath.Base(cmd.Path) != "ps" {
		t.Fatalf("path = %q, want ps executable", cmd.Path)
	}
	if !containsEnvEntry(cmd.Env, "LC_ALL=C") {
		t.Fatalf("expected LC_ALL=C in env, got %v", cmd.Env)
	}
	if !containsEnvEntry(cmd.Env, "LANG=C") {
		t.Fatalf("expected LANG=C in env, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LC_ALL") != 1 {
		t.Fatalf("expected one LC_ALL entry, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LANG") != 1 {
		t.Fatalf("expected one LANG entry, got %v", cmd.Env)
	}
}

func TestProcessStateCommandForcesCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")

	cmd := processStateCommand(123)

	if got := strings.Join(cmd.Args[1:], " "); got != "-p 123 -o stat=" {
		t.Fatalf("args = %q, want %q", got, "-p 123 -o stat=")
	}
	if filepath.Base(cmd.Path) != "ps" {
		t.Fatalf("path = %q, want ps executable", cmd.Path)
	}
	if !containsEnvEntry(cmd.Env, "LC_ALL=C") {
		t.Fatalf("expected LC_ALL=C in env, got %v", cmd.Env)
	}
	if !containsEnvEntry(cmd.Env, "LANG=C") {
		t.Fatalf("expected LANG=C in env, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LC_ALL") != 1 {
		t.Fatalf("expected one LC_ALL entry, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LANG") != 1 {
		t.Fatalf("expected one LANG entry, got %v", cmd.Env)
	}
}

func TestProcessRunningTreatsZombieAsNotRunning(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		running, err := processRunning(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("processRunning returned error: %v", err)
		}
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected zombie pid %d to be treated as not running", cmd.Process.Pid)
}

func containsEnvEntry(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func countEnvEntries(env []string, key string) int {
	prefix := key + "="
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			count++
		}
	}
	return count
}

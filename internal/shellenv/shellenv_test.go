package shellenv

import (
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestResolve_UsesLoginShellAndCapturesEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/bash")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()

	var gotShell string
	var gotArgs []string
	shellCommandOutput = func(shell string, args ...string) ([]byte, error) {
		gotShell = shell
		gotArgs = append([]string(nil), args...)
		return []byte("PATH=/resolved/bin\x00HOME=/Users/test\x00SPECIAL=1\x00"), nil
	}

	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if gotShell != "/bin/bash" {
		t.Fatalf("shell = %q, want %q", gotShell, "/bin/bash")
	}
	if !reflect.DeepEqual(gotArgs, []string{"-l", "-i", "-c", "env -0"}) {
		t.Fatalf("shell args = %v", gotArgs)
	}
	for _, want := range []string{"PATH=/resolved/bin", "HOME=/Users/test", "SPECIAL=1"} {
		if !containsEnvEntry(env, want) {
			t.Fatalf("expected resolved env to contain %q, got %v", want, env)
		}
	}
}

func TestApplyToProcess_SetsResolvedEnvEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("KEEP_ME", "1")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()

	shellCommandOutput = func(shell string, args ...string) ([]byte, error) {
		return []byte("PATH=/resolved/bin\x00HOME=/Users/test\x00SPECIAL=1\x00"), nil
	}

	if err := ApplyToProcess(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH"); got != "/resolved/bin" {
		t.Fatalf("PATH = %q", got)
	}
	if got := os.Getenv("HOME"); got != "/Users/test" {
		t.Fatalf("HOME = %q", got)
	}
	if got := os.Getenv("SPECIAL"); got != "1" {
		t.Fatalf("SPECIAL = %q", got)
	}
	if got := os.Getenv("KEEP_ME"); got != "1" {
		t.Fatalf("KEEP_ME = %q", got)
	}
}

func TestParseEnvOutput_IgnoresShellNoiseBeforeEnv(t *testing.T) {
	env := parseEnvOutput([]byte("banner text\nPATH=/resolved/bin\x00HOME=/Users/test\x00SPECIAL=1\x00"))

	want := []string{"PATH=/resolved/bin", "HOME=/Users/test", "SPECIAL=1"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

func TestDefaultShellCommandOutput_TimesOut(t *testing.T) {
	oldTimeout := shellCommandTimeout
	defer func() {
		shellCommandTimeout = oldTimeout
	}()

	shellCommandTimeout = 20 * time.Millisecond
	start := time.Now()
	_, err := defaultShellCommandOutput("/bin/sh", "-c", "sleep 1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("command ran too long: %v", elapsed)
	}
}

func containsEnvEntry(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

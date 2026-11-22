package shellenv

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestResolve_PathAugmentationPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the process environment")
	}
	for _, tc := range []struct {
		name, control, output, wantPath string
		probeErr                        error
		augmented                       bool
	}{
		{name: "default", output: "PATH=/chosen/bin\x00", augmented: true},
		{name: "enabled", control: "1", output: "PATH=/chosen/bin\x00", augmented: true},
		{name: "unknown", control: "no", output: "PATH=/chosen/bin\x00", augmented: true},
		{name: "disabled", control: "0", output: "PATH=/chosen/bin::/chosen/bin\x00", wantPath: "/chosen/bin::/chosen/bin"},
		{name: "shell opt out", output: "PATH=/chosen/bin\x00NO_MISTAKES_PATH_AUGMENT=0\x00", wantPath: "/chosen/bin"},
		{name: "process wins", control: "1", output: "PATH=/chosen/bin\x00NO_MISTAKES_PATH_AUGMENT=0\x00", augmented: true},
		{name: "empty path", control: "0", output: "PATH=\x00", wantPath: ""},
		{name: "missing path", control: "0", output: "OTHER=1\x00", wantPath: ""},
		{name: "failure", control: "0", probeErr: errors.New("fixture failure"), augmented: true},
		{name: "empty probe", control: "0", augmented: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetForTests()
			t.Cleanup(resetForTests)
			t.Setenv("SHELL", "/synthetic/bash")
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", "/fallback/bin")
			t.Setenv("NO_MISTAKES_PATH_AUGMENT", tc.control)
			oldOutput := shellCommandOutput
			t.Cleanup(func() { shellCommandOutput = oldOutput })
			calls := 0
			shellCommandOutput = func(string, []string, ...string) ([]byte, error) {
				calls++
				return []byte(tc.output), tc.probeErr
			}
			for range 2 {
				env, err := Resolve()
				if err != nil {
					t.Fatal(err)
				}
				path, present := envValue(env, "PATH")
				if tc.augmented {
					if !strings.Contains(path, filepath.Join(os.Getenv("HOME"), ".local", "bin")) {
						t.Fatalf("PATH = %q, missing fixture user-local directory", path)
					}
				} else if path != tc.wantPath || present != (tc.name != "missing path") {
					t.Fatalf("PATH = %q, present=%v; want %q", path, present, tc.wantPath)
				}
			}
			wantCalls := 1
			if tc.probeErr != nil || tc.output == "" || tc.name == "shell opt out" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("probe calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestResolve_PathPolicyDoesNotLeakThroughCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the process environment")
	}
	resetForTests()
	t.Cleanup(resetForTests)
	t.Setenv("SHELL", "/synthetic/bash")
	t.Setenv("HOME", t.TempDir())
	oldOutput := shellCommandOutput
	t.Cleanup(func() { shellCommandOutput = oldOutput })
	calls := 0
	shellCommandOutput = func(_ string, env []string, _ ...string) ([]byte, error) {
		calls++
		if env != nil {
			return []byte("PATH=/exact/bin:/usr/bin:/bin\x00"), nil
		}
		return []byte("PATH=/exact/bin:/inherited/bin\x00"), nil
	}
	for _, control := range []string{"", "0", "1", "0"} {
		t.Setenv("NO_MISTAKES_PATH_AUGMENT", control)
		env, err := Resolve()
		if err != nil {
			t.Fatal(err)
		}
		path, _ := envValue(env, "PATH")
		if control == "0" && path != "/exact/bin:/usr/bin:/bin" {
			t.Fatalf("control=%q PATH=%q", control, path)
		}
		if control != "0" && (!strings.HasPrefix(path, "/exact/bin:/inherited/bin:") || !strings.Contains(path, filepath.Join(os.Getenv("HOME"), ".local", "bin"))) {
			t.Fatalf("control=%q PATH=%q", control, path)
		}
		// A caller may mutate its copy without changing either cached policy.
		env[0] = "PATH=/caller/mutation"
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NO_MISTAKES_PATH_AUGMENT", "1")
	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if path, _ := envValue(env, "PATH"); strings.Contains(path, os.Getenv("HOME")) {
		t.Fatalf("cached default PATH changed with HOME: %q", path)
	}
	if calls != 2 {
		t.Fatalf("probe calls = %d, want 2", calls)
	}
}

func TestApplyToProcess_PathOptOutCannotResolveAugmentedTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the process environment")
	}
	resetForTests()
	t.Cleanup(resetForTests)
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(bin, "nm-fixture-path-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/synthetic/bash")
	t.Setenv("PATH", "")
	t.Setenv("NO_MISTAKES_PATH_AUGMENT", "0")
	oldOutput := shellCommandOutput
	t.Cleanup(func() { shellCommandOutput = oldOutput })
	exact := t.TempDir()
	shellCommandOutput = func(string, []string, ...string) ([]byte, error) {
		return []byte("PATH=" + exact + "\x00NO_MISTAKES_PATH_AUGMENT=1\x00"), nil
	}
	for range 2 {
		if err := ApplyToProcess(); err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv("PATH"); got != exact {
			t.Fatalf("PATH=%q, want %q", got, exact)
		}
		if _, err := exec.LookPath("nm-fixture-path-tool"); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("unexpected tool lookup: %v", err)
		}
	}
	t.Setenv("NO_MISTAKES_PATH_AUGMENT", "1")
	if err := ApplyToProcess(); err != nil {
		t.Fatal(err)
	}
	if got, err := exec.LookPath("nm-fixture-path-tool"); err != nil || got != tool {
		t.Fatalf("default tool lookup = %q, %v; want %q", got, err, tool)
	}
}

func TestResolve_PathOptOutProbeFailureDoesNotPoisonCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the process environment")
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "failed", err: errors.New("fixture failure")},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetForTests()
			t.Cleanup(resetForTests)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SHELL", "/synthetic/bash")
			t.Setenv("PATH", "/fallback/bin")
			t.Setenv("NO_MISTAKES_PATH_AUGMENT", "")
			oldOutput := shellCommandOutput
			t.Cleanup(func() { shellCommandOutput = oldOutput })
			failing := true
			calls := 0
			shellCommandOutput = func(_ string, env []string, _ ...string) ([]byte, error) {
				calls++
				if env == nil {
					return []byte("PATH=/inherited/bin\x00NO_MISTAKES_PATH_AUGMENT=0\x00"), nil
				}
				if failing {
					return nil, tc.err
				}
				return []byte("PATH=/chosen/bin\x00NO_MISTAKES_PATH_AUGMENT=0\x00"), nil
			}
			for range 2 {
				env, err := Resolve()
				if err != nil {
					t.Fatal(err)
				}
				path, _ := envValue(env, "PATH")
				if !strings.HasPrefix(path, "/fallback/bin:") || !strings.Contains(path, filepath.Join(os.Getenv("HOME"), ".local", "bin")) {
					t.Fatalf("fallback PATH=%q", path)
				}
			}
			failing = false
			for range 2 {
				env, err := Resolve()
				if err != nil {
					t.Fatal(err)
				}
				if path, _ := envValue(env, "PATH"); path != "/chosen/bin" {
					t.Fatalf("recovered PATH=%q, want /chosen/bin", path)
				}
			}
			if calls != 4 {
				t.Fatalf("probe calls=%d, want 4", calls)
			}
		})
	}
}

func TestResolve_PathPolicyWindowsShortCircuit(t *testing.T) {
	resetForTests()
	t.Cleanup(resetForTests)
	oldOS, oldOutput := runtimeGOOS, shellCommandOutput
	t.Cleanup(func() { runtimeGOOS, shellCommandOutput = oldOS, oldOutput })
	runtimeGOOS = "windows"
	shellCommandOutput = func(string, []string, ...string) ([]byte, error) {
		t.Fatal("Windows must never probe a shell")
		return nil, nil
	}
	for _, control := range []string{"", "0", "1"} {
		resetForTests()
		t.Setenv("NO_MISTAKES_PATH_AUGMENT", control)
		want := os.Environ()
		for range 2 {
			got, err := Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Windows environment changed for control=%q", control)
			}
		}
	}
}

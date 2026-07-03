package git

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// resolveEnv collapses a KEY=VALUE slice into a map using last-wins semantics,
// matching how exec resolves duplicate keys.
func resolveEnv(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

func TestNonInteractiveEnv_SetsGitOverrides(t *testing.T) {
	got := resolveEnv(NonInteractiveEnv(""))

	want := map[string]string{
		"GIT_EDITOR":          "true",
		"GIT_SEQUENCE_EDITOR": "true",
		"GIT_TERMINAL_PROMPT": "0",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestNonInteractiveEnv_OverridesAmbientEditor(t *testing.T) {
	t.Setenv("GIT_EDITOR", "vim")
	t.Setenv("GIT_SEQUENCE_EDITOR", "nano")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	got := resolveEnv(NonInteractiveEnv(""))

	if got["GIT_EDITOR"] != "true" {
		t.Errorf("GIT_EDITOR = %q, want \"true\" (ambient vim must be overridden)", got["GIT_EDITOR"])
	}
	if got["GIT_SEQUENCE_EDITOR"] != "true" {
		t.Errorf("GIT_SEQUENCE_EDITOR = %q, want \"true\"", got["GIT_SEQUENCE_EDITOR"])
	}
	if got["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want \"0\"", got["GIT_TERMINAL_PROMPT"])
	}
}

func TestNonInteractiveEnv_PreservesAmbientEnv(t *testing.T) {
	t.Setenv("NM_ENV_PROBE_XYZ", "kept")

	got := resolveEnv(NonInteractiveEnv(""))

	if got["NM_ENV_PROBE_XYZ"] != "kept" {
		t.Errorf("ambient env not preserved: NM_ENV_PROBE_XYZ = %q, want \"kept\"", got["NM_ENV_PROBE_XYZ"])
	}
}

// TestNonInteractiveEnv_SetsPWDToDir locks in the PWD coupling. Assigning
// cmd.Env disables os/exec's automatic PWD=Cmd.Dir injection, so the helper
// must restore it; otherwise os.Getwd in the child resolves symlinks (e.g.
// /tmp -> /private/tmp on macOS) and reports a different working directory.
func TestNonInteractiveEnv_SetsPWDToDir(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")

	got := resolveEnv(NonInteractiveEnv("/work/dir"))

	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		if got["PWD"] != "/somewhere/else" {
			t.Errorf("PWD = %q, want ambient PWD on %s", got["PWD"], runtime.GOOS)
		}
		return
	}

	if got["PWD"] != "/work/dir" {
		t.Errorf("PWD = %q, want \"/work/dir\"", got["PWD"])
	}
}

// TestNonInteractiveEnv_AbsolutizesRelativeDir locks in that a relative dir is
// absolutized before injection, matching os/exec (go.dev/issue/50599). POSIX
// defines PWD as an absolute pathname; a relative value like "." propagates
// through git receive-pack into hooks, where macOS /bin/sh (bash 3.2) trusts
// it and `pwd` collapses to "." (issue #269).
func TestNonInteractiveEnv_AbsolutizesRelativeDir(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")

	got := resolveEnv(NonInteractiveEnv("."))

	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		if got["PWD"] != "/somewhere/else" {
			t.Errorf("PWD = %q, want ambient PWD on %s", got["PWD"], runtime.GOOS)
		}
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if got["PWD"] != wd {
		t.Errorf("PWD = %q, want absolute %q for relative dir \".\"", got["PWD"], wd)
	}
}

func TestNonInteractiveEnv_EmptyDirLeavesAmbientPWD(t *testing.T) {
	t.Setenv("PWD", "/ambient/pwd")

	got := resolveEnv(NonInteractiveEnv(""))

	if got["PWD"] != "/ambient/pwd" {
		t.Errorf("PWD = %q, want ambient \"/ambient/pwd\" when dir is empty", got["PWD"])
	}
}

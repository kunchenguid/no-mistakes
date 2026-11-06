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
		"GIT_OPTIONAL_LOCKS":  "0",
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

func TestNonInteractiveEnvFrom_UsesBaseAndOverridesGitKeys(t *testing.T) {
	got := resolveEnv(NonInteractiveEnvFrom([]string{
		"PATH=/custom/bin",
		"NM_ENV_PROBE_XYZ=kept",
		"GIT_TERMINAL_PROMPT=1",
	}, ""))

	if got["PATH"] != "/custom/bin" {
		t.Errorf("PATH = %q, want custom base PATH", got["PATH"])
	}
	if got["NM_ENV_PROBE_XYZ"] != "kept" {
		t.Errorf("NM_ENV_PROBE_XYZ = %q, want kept", got["NM_ENV_PROBE_XYZ"])
	}
	if got["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want noninteractive override", got["GIT_TERMINAL_PROMPT"])
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

func TestSubprocessEnvironmentPreservesRepositoryConfigSources(t *testing.T) {
	base := []string{
		"GIT_AUTHOR_NAME=Daemon", "GIT_AUTHOR_EMAIL=daemon@example.test", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=Daemon", "GIT_COMMITTER_EMAIL=daemon@example.test", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=include.path", "GIT_CONFIG_VALUE_0=/daemon/config",
		"GIT_CONFIG_KEY_99=user.name", "GIT_CONFIG_VALUE_99=Orphan",
		"GIT_CONFIG_PARAMETERS='user.name=Daemon'",
		"GIT_CONFIG_GLOBAL=/private/global", "GIT_CONFIG_NOSYSTEM=1", "PATH=/toolchain", "GH_TOKEN=credential",
	}
	if runtime.GOOS == "windows" {
		for i, entry := range base {
			key, value, _ := strings.Cut(entry, "=")
			base[i] = strings.ToLower(key) + "=" + value
		}
	}
	env := SubprocessEnvironment(base)
	want := []string{"GIT_CONFIG_GLOBAL=/private/global", "GIT_CONFIG_NOSYSTEM=1", "PATH=/toolchain", "GH_TOKEN=credential"}
	if runtime.GOOS == "windows" {
		for i, entry := range want {
			key, value, _ := strings.Cut(entry, "=")
			want[i] = strings.ToLower(key) + "=" + value
		}
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("subprocess environment = %q, want %q", env, want)
	}
}

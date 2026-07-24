package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// NonInteractiveEnv returns the environment for a subprocess that may invoke
// git, with git forced into a fully non-interactive mode. It is intended for
// cmd.Env on any subprocess that may run git (our own git calls and the coding
// agents we spawn).
//
// Without these overrides, git operations such as `git rebase --continue` or
// `git commit` open $EDITOR to confirm a commit message, and remote operations
// can block on a credential prompt. In a headless agent subprocess there is no
// TTY, so the editor or prompt hangs until the agent times out. Pointing the
// editors at `true` makes git accept the existing message immediately, and
// GIT_TERMINAL_PROMPT=0 fails fast instead of blocking on credentials. The
// overrides are appended last so they win over any ambient values (exec
// resolves duplicate keys using the last occurrence).
//
// Pass the same directory assigned to cmd.Dir (or "" when it is unset). When
// cmd.Env is left nil, os/exec injects PWD=cmd.Dir automatically; assigning
// cmd.Env disables that, so callers must thread the working directory through
// here to preserve symlinked working-directory paths (for example /tmp vs
// /private/tmp on macOS, which os.Getwd reports differently depending on PWD).
func NonInteractiveEnv(dir string) []string {
	return NonInteractiveEnvFrom(os.Environ(), dir)
}

// NonInteractiveEnvFrom is NonInteractiveEnv applied to an explicit base
// environment. A nil base means the current process environment.
func NonInteractiveEnvFrom(base []string, dir string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := append(append([]string(nil), base...),
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_TERMINAL_PROMPT=0",
		// Read-only commands such as status and rev-parse must not refresh the
		// index as a side effect. Mutating commands still take required locks.
		"GIT_OPTIONAL_LOCKS=0",
	)
	// Mirror os/exec, which only injects PWD when Cmd.Env is nil, skips it on
	// these platforms, and absolutizes Cmd.Dir first (go.dev/issue/50599):
	// POSIX defines PWD as "an absolute pathname of the current working
	// directory". Injecting a relative dir verbatim (for example ".") poisons
	// every descendant that trusts PWD — macOS /bin/sh is bash 3.2, whose pwd
	// builtin reports "." when PWD="." leaks through git receive-pack into a
	// hook, which is how the post-receive hook of issue #269 ended up passing
	// `--gate .`.
	if dir != "" && runtime.GOOS != "windows" && runtime.GOOS != "plan9" {
		if abs, err := filepath.Abs(dir); err == nil {
			env = append(env, "PWD="+abs)
		}
	}
	return env
}

// gitSpawnEnv returns NonInteractiveEnv further scoped for a git process we
// spawn ourselves (git.Run and RefExists). On Windows it appends "noglob" to
// CYGWIN/MSYS so a Cygwin- or MSYS2-linked git does not glob-expand the
// arguments we hand it (issue #427). It is a no-op off Windows.
//
// The scoping is the whole point. NonInteractiveEnv is also the base for the
// coding-agent env (agent.gitSafeEnv), and disabling globbing there would
// suppress it for every Cygwin/MSYS2 tool those agents exec, a blast radius
// wider than the git calls that motivated the fix. An earlier version injected
// noglob inside NonInteractiveEnv itself and hit exactly that; it was reverted
// in commit d36bcd9. Injecting here keeps noglob on our own git subprocesses
// only. (Agents that shell out to git through gitSafeEnv are intentionally left
// unprotected: they already ran without noglob, and we cannot enable it for
// their git without also disabling it for every other Cygwin tool they run.)
func gitSpawnEnv(dir string) []string {
	return disableChildArgGlobbing(NonInteractiveEnv(dir))
}

// disableChildArgGlobbing appends "noglob" to the CYGWIN and MSYS environment
// variables so a Cygwin- or MSYS2-linked git binary does not glob-expand the
// arguments we pass it.
//
// On Windows a native process (our Go binary) can only hand a child a single
// command-line string; Cygwin/MSYS2 programs re-parse it at startup and run
// their own globber over it. That globber strips the braces from an argument
// like `refs/heads/main^{commit}`, turning it into `refs/heads/main^commit`,
// which git then rejects as an ambiguous argument (issue #427); it would also
// expand any bare `*`, `?`, or `[...]` we passed literally. From a Cygwin shell
// the braces survive because argv is passed through Cygwin's own exec, which is
// why the failure only shows up when our native daemon spawns git.
//
// We always pass git explicit, already-resolved arguments and never rely on
// runtime globbing, so disabling it is safe. Any options the user already set
// in CYGWIN/MSYS are preserved; "noglob" is appended only when absent. This is
// a no-op off Windows, where these variables have no meaning.
func disableChildArgGlobbing(env []string) []string {
	if runtime.GOOS != "windows" {
		return env
	}
	for _, key := range []string{"CYGWIN", "MSYS"} {
		existing := lastEnvValue(env, key)
		if containsWord(existing, "noglob") {
			continue
		}
		value := "noglob"
		if strings.TrimSpace(existing) != "" {
			value = existing + " noglob"
		}
		env = append(env, key+"="+value)
	}
	return env
}

// lastEnvValue returns the value of the last KEY=VALUE entry for key in env,
// matching the last-wins semantics os/exec uses for duplicate keys. The key is
// compared case-insensitively: Windows environment variable names are
// case-insensitive, so an ambient "Cygwin=winsymlinks:native" must be found and
// preserved rather than shadowed by a freshly appended uppercase "CYGWIN=noglob"
// (which os/exec would let win under its own case-insensitive last-wins dedup).
func lastEnvValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		name, val, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, key) {
			value = val
		}
	}
	return value
}

// containsWord reports whether space-separated value already contains word.
func containsWord(value, word string) bool {
	for _, field := range strings.Fields(value) {
		if field == word {
			return true
		}
	}
	return false
}

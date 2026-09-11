package shellenv

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var runtimeGOOS = runtime.GOOS
var lookupEnv = os.LookupEnv
var currentUser = user.Current
var shellCommandOutput = defaultShellCommandOutput

// shellCommandTimeout bounds each login-shell probe. It is deliberately
// forgiving: a 2s budget was too tight for an interactive login shell under
// cold-start/system load, and exceeding it silently dropped the daemon onto a
// degraded fallback PATH that omitted version-manager dirs (nvm/fnm/volta),
// the intermittent trigger behind #143.
var shellCommandTimeout = 30 * time.Second

// DefaultShellRetryWindow is how long a daemon startup keeps re-probing while
// the login shell binary itself is missing. The window only needs to cover a
// boot-time race: on macOS with nix-darwin the login shell lives under
// /run/current-system, a symlink that the org.nixos.activate-system daemon
// recreates only after the Nix store volume is mounted, and a RunAtLoad launch
// agent can start before that. The probe then fails with fork/exec ENOENT in
// about a millisecond, so waiting is the only way to see the real shell.
const DefaultShellRetryWindow = 60 * time.Second

// shellRetrySleep is the retry sleeper, swapped by tests so retry behavior is
// asserted without wall-clock waits.
var shellRetrySleep = time.Sleep

const (
	shellRetryInitialBackoff = time.Second
	shellRetryMaxBackoff     = 10 * time.Second
)

var cacheMu sync.Mutex
var cachedEnv []string
var degraded bool

func LoginShell() string {
	if shell, ok := lookupEnv("SHELL"); ok && strings.TrimSpace(shell) != "" {
		return shell
	}
	if runtimeGOOS == "darwin" {
		if shell := shellFromDSCL(); shell != "" {
			return shell
		}
	}
	if runtimeGOOS == "linux" {
		if shell := shellFromGetent(); shell != "" {
			return shell
		}
	}
	return "bash"
}

func SupportsInteractive(shell string) bool {
	base := filepath.Base(shell)
	return base == "bash" || base == "zsh"
}

// Resolve returns the login shell environment, probing the shell at most once
// and falling back to the augmented process environment when the probe fails.
func Resolve() ([]string, error) {
	return ResolveWithShellRetry(0)
}

// ResolveWithShellRetry is Resolve for callers that can afford to wait for a
// login shell binary that does not exist yet (see DefaultShellRetryWindow).
// Only that failure is retried: a shell that exists but fails or prints no
// environment is not going to heal by waiting, and blocking on it would only
// delay startup. The retry sleeps add up to at most window.
func ResolveWithShellRetry(window time.Duration) ([]string, error) {
	cacheMu.Lock()
	if cachedEnv != nil {
		defer cacheMu.Unlock()
		return append([]string(nil), cachedEnv...), nil
	}
	cacheMu.Unlock()

	resolved, resolvedFromShell := resolveUncached(window)

	// Only cache a successful shell resolution. A degraded fallback (the shell
	// probe failed or returned nothing) must never be cached: one bad daemon
	// startup would otherwise poison every spawned agent for the daemon's whole
	// lifetime - the failure mode behind #143. Leaving it uncached lets a later
	// call (the daemon re-probes at run start while Degraded) recover the real
	// login-shell PATH.
	cacheMu.Lock()
	if resolvedFromShell && cachedEnv == nil {
		cachedEnv = append([]string(nil), resolved...)
	}
	degraded = !resolvedFromShell
	cacheMu.Unlock()
	return append([]string(nil), resolved...), nil
}

// Degraded reports whether the most recent resolution fell back to the
// augmented process environment instead of a successful login-shell probe. It
// is false before any resolution and after a successful one.
func Degraded() bool {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return degraded
}

func ApplyToProcess() error {
	return ApplyToProcessWithShellRetry(0)
}

// ApplyToProcessWithShellRetry applies ResolveWithShellRetry(window) to the
// current process environment.
func ApplyToProcessWithShellRetry(window time.Duration) error {
	env, err := ResolveWithShellRetry(window)
	if err != nil {
		return err
	}
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if key == "" {
			continue
		}
		if !found {
			value = ""
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	return nil
}

// errNoEntries marks a probe whose shell ran but printed no parseable
// environment entries.
var errNoEntries = errors.New("login shell printed no environment entries")

// resolveUncached probes the login shell for its environment, waiting up to
// window for a missing shell binary to appear. The bool return reports whether
// the result came from a successful shell probe (true) or a degraded fallback
// (false); callers use it to decide whether the result is safe to cache.
func resolveUncached(window time.Duration) ([]string, bool) {
	if runtimeGOOS == "windows" {
		return append([]string(nil), os.Environ()...), true
	}

	shell := LoginShell()
	resolved, err := probeLoginShell(shell)
	backoff := shellRetryInitialBackoff
	for attempt := 1; shellBinaryMissing(err) && backoff <= window; attempt++ {
		slog.Warn("login shell binary is missing, waiting for it before resolving the login shell environment (#143)",
			"shell", shell, "attempt", attempt, "backoff", backoff, "err", err)
		shellRetrySleep(backoff)
		window -= backoff
		if backoff *= 2; backoff > shellRetryMaxBackoff {
			backoff = shellRetryMaxBackoff
		}
		resolved, err = probeLoginShell(shell)
	}
	switch {
	case errors.Is(err, errNoEntries):
		slog.Warn("login shell environment resolution returned no entries; using a degraded fallback PATH that may omit version-manager dirs (nvm/fnm/volta), so agents may not find tools like pnpm (#143)",
			"shell", shell)
	case err != nil:
		slog.Warn("login shell environment resolution failed; using a degraded fallback PATH that may omit version-manager dirs (nvm/fnm/volta), so agents may not find tools like pnpm (#143)",
			"shell", shell, "err", err)
	}
	if err != nil {
		fallback := append([]string(nil), os.Environ()...)
		return augmentPath(ensureShellEntry(fallback, shell)), false
	}
	return augmentPath(ensureShellEntry(resolved, shell)), true
}

// probeLoginShell runs the login shell once and returns its environment.
// Interactive mode is deliberate for bash and zsh: version managers such as
// nvm, fnm, and volta are sourced from .zshrc/.bashrc, not the profile.
func probeLoginShell(shell string) ([]string, error) {
	args := []string{"-l", "-c", "env -0"}
	if SupportsInteractive(shell) {
		args = []string{"-l", "-i", "-c", "env -0"}
	}
	out, err := shellCommandOutput(shell, args...)
	if err != nil {
		return nil, err
	}
	resolved := parseEnvOutput(out)
	if len(resolved) == 0 {
		return nil, errNoEntries
	}
	return resolved, nil
}

// shellBinaryMissing reports whether the probe failed because the shell
// executable itself is absent (fork/exec ENOENT for an absolute path, or an
// unresolvable bare name), the only failure that waiting can fix.
func shellBinaryMissing(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound))
}

// WellKnownBinDirs returns common binary install locations that should be on
// PATH for tools like Homebrew, user-local installs, and language package
// managers (Go, Rust). Non-existent directories are included unchanged
// because Go's exec.LookPath ignores missing PATH entries - filtering here
// would require filesystem access in resolution, which complicates testing
// without adding value for daemon launch.
func WellKnownBinDirs() []string {
	return WellKnownBinDirsForHome(homeDir())
}

func WellKnownBinDirsForHome(home string) []string {
	dirs := []string{}
	if strings.TrimSpace(home) != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "go", "bin"),
			filepath.Join(home, ".cargo", "bin"),
			filepath.Join(home, "bin"),
		)
	}
	dirs = append(dirs,
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	)
	return dirs
}

func homeDir() string {
	if home, ok := lookupEnv("HOME"); ok && strings.TrimSpace(home) != "" {
		return home
	}
	u, err := currentUser()
	if err != nil || u == nil {
		return ""
	}
	return strings.TrimSpace(u.HomeDir)
}

// augmentPath merges WellKnownBinDirs into the PATH entry of env, preserving
// existing entries in order (so user-configured PATH continues to win) and
// appending any well-known dirs that are not already present. If env lacks a
// PATH entry entirely, one is synthesized from WellKnownBinDirs.
func augmentPath(env []string) []string {
	sep := string(os.PathListSeparator)
	pathIdx := -1
	var existing []string
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			pathIdx = i
			raw := strings.TrimPrefix(entry, "PATH=")
			if raw != "" {
				existing = strings.Split(raw, sep)
			}
			break
		}
	}
	seen := make(map[string]struct{}, len(existing))
	for _, p := range existing {
		seen[p] = struct{}{}
	}
	for _, d := range WellKnownBinDirs() {
		if _, ok := seen[d]; ok {
			continue
		}
		existing = append(existing, d)
		seen[d] = struct{}{}
	}
	merged := "PATH=" + strings.Join(existing, sep)
	if pathIdx >= 0 {
		env[pathIdx] = merged
	} else {
		env = append(env, merged)
	}
	return env
}

func parseEnvOutput(out []byte) []string {
	parts := strings.Split(string(out), "\x00")
	env := make([]string, 0, len(parts))
	for _, part := range parts {
		entry, ok := parseEnvEntry(part)
		if !ok {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func parseEnvEntry(part string) (string, bool) {
	if part == "" {
		return "", false
	}
	candidateStarts := []int{0}
	for i := 0; i < len(part); i++ {
		if part[i] == '\n' || part[i] == '\r' {
			candidateStarts = append(candidateStarts, i+1)
		}
	}
	for _, start := range candidateStarts {
		candidate := strings.TrimLeft(part[start:], "\r\n")
		if candidate == "" {
			continue
		}
		key, _, found := strings.Cut(candidate, "=")
		if found && validEnvKey(key) {
			return candidate, true
		}
	}
	return "", false
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 {
			if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

func ensureShellEntry(env []string, shell string) []string {
	for _, entry := range env {
		if strings.HasPrefix(entry, "SHELL=") {
			return env
		}
	}
	return append(env, "SHELL="+shell)
}

func shellFromDSCL() string {
	username := currentUsername()
	if username == "" {
		return ""
	}
	out, err := shellCommandOutput("dscl", ".", "-read", "/Users/"+username, "UserShell")
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	line = strings.TrimPrefix(line, "UserShell:")
	return strings.TrimSpace(line)
}

func shellFromGetent() string {
	username := currentUsername()
	if username == "" {
		return ""
	}
	out, err := shellCommandOutput("getent", "passwd", username)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return ""
	}
	parts := strings.Split(line, ":")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[len(parts)-1])
}

func currentUsername() string {
	if username, ok := lookupEnv("USER"); ok && strings.TrimSpace(username) != "" {
		return username
	}
	u, err := currentUser()
	if err != nil || u == nil {
		return ""
	}
	return strings.TrimSpace(u.Username)
}

func defaultShellCommandOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), shellCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	ConfigureShellCommand(cmd)
	detachFromTerminal(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	return OutputShellCommand(cmd)
}

func resetForTests() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cachedEnv = nil
	degraded = false
}

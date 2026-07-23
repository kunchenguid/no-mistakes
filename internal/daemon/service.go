package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Base identifiers for the managed-service artifacts. The live identifiers
// returned by launchdServiceLabel/systemdServiceName/windowsTaskName include
// a short stable suffix derived from p.Root() so two no-mistakes installs
// with different NM_HOMEs cannot collide in the global launchctl/systemctl/
// schtasks namespace. See serviceInstanceSuffix for the full rationale.
const (
	launchdServiceLabelBase = "com.kunchenguid.no-mistakes.daemon"
	systemdServiceNameBase  = "no-mistakes-daemon"
	windowsTaskNameBase     = "no-mistakes-daemon"
)

// Legacy (pre-scoping) identifiers, retained only so that a new binary can
// clean up artifacts installed by a pre-fix binary on first `daemon start`.
const (
	legacyLaunchdServiceLabel = "com.kunchenguid.no-mistakes.daemon"
	legacySystemdServiceName  = "no-mistakes-daemon.service"
	legacyWindowsTaskName     = "no-mistakes-daemon"
)

var runtimeGOOS = runtime.GOOS
var serviceUserHomeDir = os.UserHomeDir
var serviceCurrentUser = user.Current
var serviceExecutablePath = os.Executable
var serviceCommandRunner = runServiceCommand
var serviceManagerBypassed = defaultServiceManagerBypassed
var inspectManagedDaemonService = managedDaemonServiceState

type managedServiceState int

const (
	managedServiceUnknown managedServiceState = iota
	managedServiceRunning
	managedServiceExited
)

func managedDaemonServiceState(p *paths.Paths) (managedServiceState, error) {
	if serviceManagerBypassed() {
		return managedServiceUnknown, nil
	}
	switch runtimeGOOS {
	case "darwin":
		domain, err := launchdDomainTarget()
		if err != nil {
			return managedServiceUnknown, err
		}
		output, err := serviceCommandRunner("launchctl", "print", domain+"/"+launchdServiceLabel(p))
		if err != nil {
			return managedServiceExited, nil
		}
		text := strings.ToLower(string(output))
		if strings.Contains(text, "state = running") || strings.Contains(text, "pid =") {
			return managedServiceRunning, nil
		}
		if strings.Contains(text, "last exit code =") || strings.Contains(text, "state = exited") {
			return managedServiceExited, nil
		}
	case "linux":
		output, err := serviceCommandRunner(
			"systemctl", "--user", "show", systemdServiceName(p),
			"--property=ActiveState", "--property=SubState", "--property=MainPID",
			"--property=ExecMainCode", "--property=ExecMainStatus", "--property=Result",
		)
		if err != nil {
			return managedServiceUnknown, err
		}
		values := make(map[string]string)
		for _, line := range strings.Split(string(output), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				values[key] = value
			}
		}
		if values["ActiveState"] == "active" && values["MainPID"] != "" && values["MainPID"] != "0" {
			return managedServiceRunning, nil
		}
		if values["ActiveState"] == "failed" || values["ActiveState"] == "inactive" ||
			values["Result"] == "exit-code" || values["Result"] == "signal" {
			return managedServiceExited, nil
		}
	case "windows":
		output, err := serviceCommandRunner("schtasks", "/Query", "/TN", windowsTaskName(p), "/FO", "LIST", "/V")
		if err != nil {
			return managedServiceExited, nil
		}
		text := strings.ToLower(string(output))
		if strings.Contains(text, "status:") && strings.Contains(text, "running") {
			return managedServiceRunning, nil
		}
		if strings.Contains(text, "status:") && (strings.Contains(text, "ready") || strings.Contains(text, "could not start")) {
			return managedServiceExited, nil
		}
	}
	return managedServiceUnknown, nil
}

// proxyEnvKeys are the proxy-related variables forwarded into the managed
// daemon's environment. A daemon started by systemd/launchd inherits only a
// minimal environment (HOME and a curated PATH), so without forwarding these
// the daemon - and the agents it spawns, e.g. `claude --print` - cannot reach
// the network through a corporate or local HTTP(S) proxy and fail with errors
// like "403 Request not allowed". Both upper- and lower-case spellings are
// forwarded because tooling is inconsistent about which it reads: curl, for
// instance, honours only the lower-case http_proxy for plain-HTTP requests (it
// deliberately ignores HTTP_PROXY to avoid the CGI "httpoxy" issue), while many
// other tools read the upper-case names. serviceProxyEnv only collapses the two
// spellings on Windows, where they are the same variable.
var proxyEnvKeys = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
}

// serviceProxyEnv returns the proxy-related environment entries present in the
// current (install-time) environment as ordered key/value pairs, skipping any
// that are unset or empty. Baking these into the generated service definition
// lets the managed daemon reach the network through the same proxy the user
// installed with, even when the login-shell environment probe is unavailable.
//
// On case-sensitive platforms (macOS, Linux) every spelling that is set is
// forwarded verbatim under its own name. That preserves the contract a
// lower-case-only consumer such as curl relies on: a value exported only as
// http_proxy must reach the daemon as http_proxy, not normalised to HTTP_PROXY.
//
// Windows is the one supported platform whose environment-variable names are
// case-insensitive: os.LookupEnv("HTTP_PROXY") and os.LookupEnv("http_proxy")
// resolve to the same variable, so iterating both spellings would otherwise
// bake a duplicate entry (HTTP_PROXY and http_proxy with identical values) into
// the rendered service definition. There the spellings are de-duplicated
// case-insensitively, keeping the first (upper-case) occurrence; this is
// harmless because Windows consumers read the variable case-insensitively too.
func serviceProxyEnv() [][2]string {
	// Only Windows treats environment-variable names case-insensitively; macOS
	// and Linux keep HTTP_PROXY and http_proxy as distinct variables.
	caseInsensitiveEnv := runtimeGOOS == "windows"
	var out [][2]string
	seen := make(map[string]bool, len(proxyEnvKeys))
	for _, key := range proxyEnvKeys {
		if caseInsensitiveEnv && seen[strings.ToUpper(key)] {
			continue
		}
		value, ok := os.LookupEnv(key)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		out = append(out, [2]string{key, value})
		if caseInsensitiveEnv {
			seen[strings.ToUpper(key)] = true
		}
	}
	return out
}

// writeServiceFile renders a generated service definition (systemd unit or
// launchd plist) via render and writes it with a permission mode that depends
// on whether proxy values were forwarded into it. When proxy variables are
// present the file may carry sensitive data (a proxy URL can embed credentials,
// e.g. http://user:pass@host), so it is restricted to owner-only 0600;
// otherwise the conventional 0644 is kept so non-proxy installs are
// byte-for-byte and mode-for-mode unchanged.
//
// The proxy environment is resolved here, exactly once, and handed to render so
// that the rendered content and the permission mode are derived from the same
// value and cannot disagree. Taking a render callback rather than a finished
// (content, proxyEnv) pair makes it structurally impossible for a caller to
// bake proxy credentials into the content yet have it written under the
// world-readable 0644 mode.
//
// When the current environment has no proxy variables set but a proxy was
// already baked into the existing on-disk definition, the proxy is inherited
// from that file via parseExistingProxyEnv rather than stripped. This mirrors
// how reinstallManagedServiceIfChanged inherits the existing executable, and
// keeps an env-less reinstall (e.g. a binary upgrade) from dropping the proxy
// the daemon relies on. parseExistingProxyEnv may be nil for callers that never
// forward a proxy.
//
// When proxy values are present the content is written to a sibling temp file
// created at 0600 and atomically renamed over the target, so credential-bearing
// content is owner-only from the instant it first exists on disk. A plain
// os.WriteFile only applies its mode on create, so re-installing over a
// pre-existing 0644 file (the no-proxy -> proxy transition) would leave the
// credentials world-readable until a follow-up Chmod tightened the mode.
func writeServiceFile(path string, parseExistingProxyEnv func([]byte) [][2]string, render func(proxyEnv [][2]string) string) error {
	proxyEnv := serviceProxyEnv()
	if len(proxyEnv) == 0 && parseExistingProxyEnv != nil {
		if existing, err := os.ReadFile(path); err == nil {
			proxyEnv = parseExistingProxyEnv(existing)
		}
	}
	content := []byte(render(proxyEnv))
	if len(proxyEnv) == 0 {
		mode := os.FileMode(0o644)
		if err := os.WriteFile(path, content, mode); err != nil {
			return err
		}
		return os.Chmod(path, mode)
	}
	return writeFileAtomic(path, content, 0o600)
}

// writeFileAtomic writes content to a temp file created at mode in the target's
// directory and atomically renames it over path. The content therefore never
// exists on disk under a mode looser than requested - even when path already
// exists with a different mode, where a plain os.WriteFile would preserve the
// existing file's mode and only a follow-up Chmod would tighten it.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

// defaultServiceManagerBypassed reports whether managed-service plumbing
// (launchctl/systemctl/schtasks) should be skipped.
//
// It returns true when NM_TEST_START_DAEMON=1 is set (the production escape
// hatch used by demo recordings and similar) or when the process is running
// under `go test`. The test-binary guard is critical because the managed
// service label, plist path, systemd unit path, and schtasks task name are
// all globally scoped under the current user - they do not honor the
// *paths.Paths argument. Without this guard, any daemon test that calls
// Start/Stop with an unstubbed paths.Paths would reach into the developer's
// real ~/Library/LaunchAgents (or systemd user unit dir, or scheduled tasks)
// and tear down a live daemon. Tests that specifically want to exercise the
// managed path (service_test.go) override serviceManagerBypassed via
// stubServiceRuntime.
func defaultServiceManagerBypassed() bool {
	if os.Getenv("NM_TEST_START_DAEMON") == "1" {
		return true
	}
	return testing.Testing()
}

// serviceInstanceSuffix returns a short stable suffix derived from p.Root()
// so managed-service artifacts (launchd label + plist filename, systemd unit
// name + path, Windows task name) are scoped per-install instead of sharing
// a single globally unique identifier per user.
//
// Without scoping, the launchd label com.kunchenguid.no-mistakes.daemon (and
// its systemd/Windows equivalents) is a shared slot. Any no-mistakes process
// on the machine can `launchctl bootout gui/<uid>/com.kunchenguid.no-mistakes.daemon`
// and tear down another install's daemon. The failure mode observed twice in
// practice: a pipeline review step ran `go test ./internal/daemon` in a
// worktree, that test binary reached TestStopNotRunningIsNoop which calls
// Stop(p) on a tmpdir paths.Paths, Stop() resolved to the global launchctl
// label, and the live LaunchAgent-managed daemon was SIGTERM'd.
//
// By scoping every identifier by sha256(p.Root()), the test's Stop(p)
// inspects a path and label that belong to its own tmpdir, not the live
// daemon's NM_HOME. managedServiceInstalled(p) stats a non-existent scoped
// plist, returns false, and Stop never reaches serviceCommandRunner.
//
// A secondary benefit: multiple concurrent NM_HOMEs (e.g. a dev vs prod
// no-mistakes install) each get their own managed daemon and can coexist.
func serviceInstanceSuffix(p *paths.Paths) string {
	root := ""
	if p != nil {
		root = p.Root()
	}
	sum := sha256.Sum256([]byte(canonicalRoot(root)))
	return hex.EncodeToString(sum[:4])
}

// canonicalRoot collapses a root path to its stable physical form so two
// spellings of the same directory (absolute vs relative, symlinked vs real,
// trailing-slash variants, and case on Windows) compare equal. It mirrors the
// normalization serviceInstanceSuffix hashes, so the managed-service identifier
// and the daemon-collision key (reconcileCollidingDaemons) agree on what
// "same root" means. Resolution is best-effort: if symlinks cannot be evaluated
// the cleaned input is returned unchanged.
func canonicalRoot(root string) string {
	if root == "" {
		return ""
	}
	if !filepath.IsAbs(root) {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	root = filepath.Clean(root)
	if runtimeGOOS == "windows" {
		root = strings.ToLower(root)
	}
	return root
}

func launchdServiceLabel(p *paths.Paths) string {
	return launchdServiceLabelBase + "." + serviceInstanceSuffix(p)
}

func systemdServiceName(p *paths.Paths) string {
	return systemdServiceNameBase + "-" + serviceInstanceSuffix(p) + ".service"
}

func windowsTaskName(p *paths.Paths) string {
	return windowsTaskNameBase + "-" + serviceInstanceSuffix(p)
}

func installManagedService(p *paths.Paths) (bool, error) {
	if serviceManagerBypassed() {
		return false, nil
	}
	exe, err := serviceExecutablePath()
	if err != nil {
		return false, fmt.Errorf("resolve executable: %w", err)
	}
	return installManagedServiceWithExecutable(p, exe)
}

func installManagedServiceWithExecutable(p *paths.Paths, exe string) (bool, error) {
	switch runtimeGOOS {
	case "darwin":
		return true, installLaunchAgent(p, exe)
	case "linux":
		return true, installSystemdUserService(p, exe)
	case "windows":
		return true, installWindowsTask(p, exe)
	default:
		return false, nil
	}
}

func startManagedService(p *paths.Paths) (bool, error) {
	if serviceManagerBypassed() {
		return false, nil
	}
	switch runtimeGOOS {
	case "darwin":
		return true, startLaunchAgent(p)
	case "linux":
		return true, startSystemdUserService(p)
	case "windows":
		return true, startWindowsTask(p)
	default:
		return false, nil
	}
}

func restartManagedService(p *paths.Paths) (bool, error) {
	if serviceManagerBypassed() {
		return false, nil
	}
	switch runtimeGOOS {
	case "darwin":
		return true, startLaunchAgent(p)
	case "linux":
		return true, restartSystemdUserService(p)
	default:
		return startManagedService(p)
	}
}

func reloadManagedServiceDefinition(p *paths.Paths) error {
	if serviceManagerBypassed() {
		return nil
	}
	switch runtimeGOOS {
	case "linux":
		_, err := serviceCommandRunner("systemctl", "--user", "daemon-reload")
		if err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w", err)
		}
	}
	return nil
}

func stopManagedService(p *paths.Paths) (bool, error) {
	if serviceManagerBypassed() || !managedServiceInstalled(p) {
		return false, nil
	}
	switch runtimeGOOS {
	case "darwin":
		return true, stopLaunchAgent(p)
	case "linux":
		return true, stopSystemdUserService(p)
	case "windows":
		return true, stopWindowsTask(p)
	default:
		return false, nil
	}
}

// resetFailedManagedService clears failed-unit bookkeeping for the daemon's
// managed service. A crash-looping systemd unit (Restart=always on a dead
// binary) is held by the manager in a failed/backoff state that keeps emitting
// journal entries even after the process is gone. Clearing it lets the fresh
// install in Start() begin from a clean slate. No-op on platforms without an
// equivalent and when the service manager is bypassed.
func resetFailedManagedService(p *paths.Paths) {
	if serviceManagerBypassed() {
		return
	}
	switch runtimeGOOS {
	case "linux":
		_, _ = serviceCommandRunner("systemctl", "--user", "reset-failed", systemdServiceName(p))
	}
}

func managedServiceInstalled(p *paths.Paths) bool {
	if serviceManagerBypassed() {
		return false
	}
	switch runtimeGOOS {
	case "darwin":
		_, err := os.Stat(launchAgentPath(p))
		return err == nil
	case "linux":
		_, err := os.Stat(systemdUserServicePath(p))
		return err == nil
	case "windows":
		if p == nil {
			return false
		}
		_, err := serviceCommandRunner("schtasks", "/Query", "/TN", windowsTaskName(p))
		return err == nil
	default:
		return false
	}
}

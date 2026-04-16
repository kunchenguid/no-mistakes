package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
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
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	root = filepath.Clean(root)
	if runtimeGOOS == "windows" {
		root = strings.ToLower(root)
	}
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:4])
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

func installLaunchAgent(p *paths.Paths, exe string) error {
	path := launchAgentPath(p)
	home, err := serviceUserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create launch agents directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderLaunchAgent(exe, p, home)), 0o644); err != nil {
		return fmt.Errorf("write launch agent: %w", err)
	}
	cleanupLegacyLaunchAgent()
	return nil
}

// cleanupLegacyLaunchAgent removes any plist installed by a pre-scoping
// binary at the globally-named path so the new scoped install is the only
// managed daemon for this user going forward. We bootout the legacy label
// before deleting so an already-loaded legacy daemon is released from
// launchd (it will exit on SIGTERM). Any error is best-effort: if there's
// no legacy plist or launchctl refuses, we proceed with the scoped install.
func cleanupLegacyLaunchAgent() {
	path := legacyLaunchAgentPath()
	if _, err := os.Stat(path); err != nil {
		return
	}
	if domain, err := launchdDomainTarget(); err == nil {
		_, _ = serviceCommandRunner("launchctl", "bootout", domain+"/"+legacyLaunchdServiceLabel)
	}
	_ = os.Remove(path)
}

func startLaunchAgent(p *paths.Paths) error {
	domain, err := launchdDomainTarget()
	if err != nil {
		return err
	}
	serviceTarget := domain + "/" + launchdServiceLabel(p)
	path := launchAgentPath(p)
	_, _ = serviceCommandRunner("launchctl", "bootout", serviceTarget)
	_, bootstrapErr := serviceCommandRunner("launchctl", "bootstrap", domain, path)
	_, kickstartErr := serviceCommandRunner("launchctl", "kickstart", "-k", serviceTarget)
	if kickstartErr != nil {
		if bootstrapErr != nil {
			return fmt.Errorf("launchctl bootstrap: %v; kickstart: %w", bootstrapErr, kickstartErr)
		}
		return fmt.Errorf("launchctl kickstart: %w", kickstartErr)
	}
	return nil
}

func stopLaunchAgent(p *paths.Paths) error {
	domain, err := launchdDomainTarget()
	if err != nil {
		return err
	}
	_, err = serviceCommandRunner("launchctl", "bootout", domain+"/"+launchdServiceLabel(p))
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w", err)
	}
	return nil
}

func installSystemdUserService(p *paths.Paths, exe string) error {
	path := systemdUserServicePath(p)
	home, err := serviceUserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create systemd user directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderSystemdUnit(exe, p, home)), 0o644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if _, err := serviceCommandRunner("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := serviceCommandRunner("systemctl", "--user", "enable", systemdServiceName(p)); err != nil {
		return fmt.Errorf("systemctl enable: %w", err)
	}
	cleanupLegacySystemdUnit()
	return nil
}

func cleanupLegacySystemdUnit() {
	path := legacySystemdUserServicePath()
	if _, err := os.Stat(path); err != nil {
		return
	}
	_, _ = serviceCommandRunner("systemctl", "--user", "stop", legacySystemdServiceName)
	_, _ = serviceCommandRunner("systemctl", "--user", "disable", legacySystemdServiceName)
	_ = os.Remove(path)
}

func startSystemdUserService(p *paths.Paths) error {
	_, err := serviceCommandRunner("systemctl", "--user", "start", systemdServiceName(p))
	if err != nil {
		return fmt.Errorf("systemctl start: %w", err)
	}
	return nil
}

func stopSystemdUserService(p *paths.Paths) error {
	_, err := serviceCommandRunner("systemctl", "--user", "stop", systemdServiceName(p))
	if err != nil {
		return fmt.Errorf("systemctl stop: %w", err)
	}
	return nil
}

func installWindowsTask(p *paths.Paths, exe string) error {
	args := []string{
		"/Create",
		"/TN", windowsTaskName(p),
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
		"/TR", buildWindowsTaskCommand(exe, p.Root()),
	}
	if _, err := serviceCommandRunner("schtasks", args...); err != nil {
		return fmt.Errorf("schtasks create: %w", err)
	}
	cleanupLegacyWindowsTask()
	return nil
}

func cleanupLegacyWindowsTask() {
	// schtasks /Query returns a non-zero exit when the task doesn't exist,
	// so we only issue the destructive Delete when the legacy task is
	// actually present.
	if _, err := serviceCommandRunner("schtasks", "/Query", "/TN", legacyWindowsTaskName); err != nil {
		return
	}
	_, _ = serviceCommandRunner("schtasks", "/End", "/TN", legacyWindowsTaskName)
	_, _ = serviceCommandRunner("schtasks", "/Delete", "/TN", legacyWindowsTaskName, "/F")
}

func startWindowsTask(p *paths.Paths) error {
	_, err := serviceCommandRunner("schtasks", "/Run", "/TN", windowsTaskName(p))
	if err != nil {
		return fmt.Errorf("schtasks run: %w", err)
	}
	return nil
}

func stopWindowsTask(p *paths.Paths) error {
	_, err := serviceCommandRunner("schtasks", "/End", "/TN", windowsTaskName(p))
	if err != nil {
		return fmt.Errorf("schtasks end: %w", err)
	}
	return nil
}

func launchAgentPath(p *paths.Paths) string {
	home, err := serviceUserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdServiceLabel(p)+".plist")
}

func systemdUserServicePath(p *paths.Paths) string {
	home, err := serviceUserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
}

func legacyLaunchAgentPath() string {
	home, _ := serviceUserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdServiceLabel+".plist")
}

func legacySystemdUserServicePath() string {
	home, _ := serviceUserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", legacySystemdServiceName)
}

func launchdDomainTarget() (string, error) {
	u, err := serviceCurrentUser()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	if u == nil || u.Uid == "" {
		return "", fmt.Errorf("resolve current user: empty uid")
	}
	return "gui/" + u.Uid, nil
}

func renderLaunchAgent(exe string, p *paths.Paths, home string) string {
	values := []string{exe, "daemon", "run", "--root", p.Root()}
	var args strings.Builder
	for _, value := range values {
		args.WriteString("    <string>")
		args.WriteString(xmlEscaped(value))
		args.WriteString("</string>\n")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>%s</string>
  </dict>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
`, xmlEscaped(launchdServiceLabel(p)), args.String(), xmlEscaped(p.Root()), xmlEscaped(home), xmlEscaped(p.DaemonLog()), xmlEscaped(p.DaemonLog()))
}

func renderSystemdUnit(exe string, p *paths.Paths, home string) string {
	command := strings.Join([]string{
		systemdEscapeArg(exe),
		systemdEscapeArg("daemon"),
		systemdEscapeArg("run"),
		systemdEscapeArg("--root"),
		systemdEscapeArg(p.Root()),
	}, " ")
	return fmt.Sprintf(`[Unit]
Description=no-mistakes background daemon

[Service]
Type=simple
ExecStart=%s
WorkingDirectory=%s
Environment=%s
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, command, systemdEscapeArg(p.Root()), strconv.Quote("HOME="+home))
}

func buildWindowsTaskCommand(exe, root string) string {
	args := []string{quoteWindowsTaskArg(exe), "daemon", "run", "--root", quoteWindowsTaskArg(root)}
	return strings.Join(args, " ")
}

func systemdEscapeArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if strings.ContainsAny(arg, " \t\n\r\"'\\") {
		return strconv.Quote(arg)
	}
	return arg
}

func quoteWindowsTaskArg(arg string) string {
	if !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	return strconv.Quote(arg)
}

func xmlEscaped(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}

func runServiceCommand(name string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", path, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

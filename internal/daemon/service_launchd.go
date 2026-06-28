package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Retry parameters for `launchctl bootstrap` after a preceding bootout.
// launchctl bootout is async: it SIGTERMs the existing service and gives
// launchd up to ~5s to finalize cleanup. During that window, bootstrap
// returns errno 37 EPROGRESS ("Operation already in progress") and there
// is no synchronous API to wait for the previous instance to fully detach.
// A stop+start sequence (which is exactly what `make install` does, and
// what `daemon restart` does) collides with this window unless bootstrap
// is retried. Exposed as package vars so tests can shrink the timings.
var (
	launchctlBootstrapRetryTimeout  = 10 * time.Second
	launchctlBootstrapRetryInterval = 200 * time.Millisecond
)

func installLaunchAgent(p *paths.Paths, exe string) error {
	path := launchAgentPath(p)
	home, err := serviceUserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create launch agents directory: %w", err)
	}
	// writeServiceFile resolves the proxy environment once and feeds it to the
	// renderer, so the plist content and its permission mode stay in sync
	// (see serviceProxyEnv / writeServiceFile).
	render := func(proxyEnv [][2]string) string {
		return renderLaunchAgentWithProxyEnv(exe, p, home, proxyEnv)
	}
	if err := writeServiceFile(path, launchAgentProxyEnv, render); err != nil {
		return fmt.Errorf("write launch agent: %w", err)
	}
	cleanupLegacyLaunchAgent(p)
	return nil
}

// cleanupLegacyLaunchAgent removes any plist installed by a pre-scoping
// binary at the globally-named path so the new scoped install is the only
// managed daemon for this user going forward. We bootout the legacy label
// before deleting so an already-loaded legacy daemon is released from
// launchd (it will exit on SIGTERM). Any error is best-effort: if there's
// no legacy plist or launchctl refuses, we proceed with the scoped install.
func cleanupLegacyLaunchAgent(p *paths.Paths) {
	path := legacyLaunchAgentPath()
	data, err := os.ReadFile(path)
	if err != nil || !serviceDefinitionMatchesRoot(data, p) {
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
	bootstrapErr := launchctlBootstrapWithRetry(domain, path)
	_, kickstartErr := serviceCommandRunner("launchctl", "kickstart", "-k", serviceTarget)
	if kickstartErr != nil {
		if bootstrapErr != nil {
			return fmt.Errorf("launchctl bootstrap: %v; kickstart: %w", bootstrapErr, kickstartErr)
		}
		return fmt.Errorf("launchctl kickstart: %w", kickstartErr)
	}
	return nil
}

// launchctlBootstrapWithRetry runs `launchctl bootstrap` and retries on
// errno 37 EPROGRESS until the bootout-cleanup window closes. Non-busy
// failures (bad plist, bad permissions) return immediately so they can
// surface through the normal managed-start fallback path.
func launchctlBootstrapWithRetry(domain, path string) error {
	deadline := time.Now().Add(launchctlBootstrapRetryTimeout)
	var lastErr error
	for {
		_, err := serviceCommandRunner("launchctl", "bootstrap", domain, path)
		if err == nil {
			return nil
		}
		if !launchctlBootstrapBusy(err) {
			return err
		}
		lastErr = err
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(launchctlBootstrapRetryInterval)
	}
}

// launchctlBootstrapBusy reports whether a bootstrap error is the
// "previous instance still unloading" race. launchctl surfaces this as
// exit 37 with stderr "Bootstrap failed: 37: Operation already in
// progress". Match both exit status and message text so we remain robust
// to launchctl output tweaks across macOS releases.
func launchctlBootstrapBusy(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "operation already in progress") ||
		strings.Contains(text, "exit status 37")
}

func stopLaunchAgent(p *paths.Paths) error {
	domain, err := launchdDomainTarget()
	if err != nil {
		return err
	}
	output, err := serviceCommandRunner("launchctl", "bootout", domain+"/"+launchdServiceLabel(p))
	if err != nil {
		if launchctlBootoutServiceNotLoaded(err, output) {
			return nil
		}
		return fmt.Errorf("launchctl bootout: %w", err)
	}
	return nil
}

func removeLaunchAgent(p *paths.Paths) error {
	err := os.Remove(launchAgentPath(p))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// launchctlBootoutServiceNotLoaded reports whether a launchctl bootout
// failure is the ESRCH case ("No such process", exit 3) that launchctl
// emits when the service label isn't currently loaded. That is semantically
// a successful stop - the service is already not running.
func launchctlBootoutServiceNotLoaded(err error, output []byte) bool {
	if err == nil {
		return false
	}
	combined := strings.ToLower(string(output) + " " + err.Error())
	return strings.Contains(combined, "no such process")
}

func launchAgentPath(p *paths.Paths) string {
	home, err := serviceUserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdServiceLabel(p)+".plist")
}

func legacyLaunchAgentPath() string {
	home, _ := serviceUserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdServiceLabel+".plist")
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

// renderLaunchAgent renders the launchd plist, resolving the proxy environment
// from the current process environment itself. It is a convenience wrapper used
// only by tests; production callers use renderLaunchAgentWithProxyEnv, because
// both the install path and drift detection resolve the proxy environment once
// (preferring the on-disk definition when the live environment has none) and
// pass it in.
func renderLaunchAgent(exe string, p *paths.Paths, home string) string {
	return renderLaunchAgentWithProxyEnv(exe, p, home, serviceProxyEnv())
}

// renderLaunchAgentWithProxyEnv renders the launchd plist using a proxy
// environment supplied by the caller (see serviceProxyEnv).
func renderLaunchAgentWithProxyEnv(exe string, p *paths.Paths, home string, proxyEnv [][2]string) string {
	values := []string{exe, "daemon", "run", "--root", p.Root()}
	var args strings.Builder
	for _, value := range values {
		args.WriteString("    <string>")
		args.WriteString(xmlEscaped(value))
		args.WriteString("</string>\n")
	}
	// Build the entire EnvironmentVariables <dict> as one self-contained block:
	// the fixed HOME/PATH entries plus the forwarded proxy variables, closed by
	// its own </dict>. Assembling the complete dict here avoids splicing a
	// proxy fragment into the template via "%s  </dict>", which depended on the
	// fragment's trailing newline and indentation lining up. Proxy variables are
	// forwarded so the daemon (and the agents it spawns) can reach the network
	// through the user's proxy. See serviceProxyEnv.
	var envDict strings.Builder
	envDict.WriteString("  <dict>\n")
	envDict.WriteString("    <key>HOME</key>\n    <string>")
	envDict.WriteString(xmlEscaped(home))
	envDict.WriteString("</string>\n")
	envDict.WriteString("    <key>PATH</key>\n    <string>")
	envDict.WriteString(xmlEscaped(managedServicePath(home)))
	envDict.WriteString("</string>\n")
	for _, kv := range proxyEnv {
		envDict.WriteString("    <key>")
		envDict.WriteString(xmlEscaped(kv[0]))
		envDict.WriteString("</key>\n    <string>")
		envDict.WriteString(xmlEscaped(kv[1]))
		envDict.WriteString("</string>\n")
	}
	envDict.WriteString("  </dict>")
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
%s
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
`, xmlEscaped(launchdServiceLabel(p)), args.String(), xmlEscaped(p.Root()), envDict.String(), xmlEscaped(p.DaemonLog()), xmlEscaped(p.DaemonLog()))
}

// managedServicePath returns a default PATH for daemons started by a service
// manager (launchd, systemd) that would otherwise inherit only the service
// manager's minimal PATH. Home-directory entries are interpolated here
// because neither plist nor systemd Environment= expands $HOME.
//
// Entry order: user-scoped dirs first so user-managed tools (go, cargo,
// ~/.local/bin) win over system packages, then Homebrew and distro defaults.
func managedServicePath(home string) string {
	return strings.Join(shellenv.WellKnownBinDirsForHome(home), string(os.PathListSeparator))
}

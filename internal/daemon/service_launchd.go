package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
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
	if err := os.WriteFile(path, []byte(renderLaunchAgent(exe, p, home)), 0o644); err != nil {
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
    <key>PATH</key>
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
`, xmlEscaped(launchdServiceLabel(p)), args.String(), xmlEscaped(p.Root()), xmlEscaped(home), xmlEscaped(managedServicePath(home)), xmlEscaped(p.DaemonLog()), xmlEscaped(p.DaemonLog()))
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

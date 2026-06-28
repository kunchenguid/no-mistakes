package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func installSystemdUserService(p *paths.Paths, exe string) error {
	path := systemdUserServicePath(p)
	home, err := serviceUserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create systemd user directory: %w", err)
	}
	// writeServiceFile resolves the proxy environment once and feeds it to the
	// renderer, so the unit content and its permission mode stay in sync
	// (see serviceProxyEnv / writeServiceFile).
	render := func(proxyEnv [][2]string) string {
		return renderSystemdUnitWithProxyEnv(exe, p, home, proxyEnv)
	}
	if err := writeServiceFile(path, systemdUnitProxyEnv, render); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if _, err := serviceCommandRunner("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := serviceCommandRunner("systemctl", "--user", "enable", systemdServiceName(p)); err != nil {
		return fmt.Errorf("systemctl enable: %w", err)
	}
	cleanupLegacySystemdUnit(p)
	return nil
}

func cleanupLegacySystemdUnit(p *paths.Paths) {
	path := legacySystemdUserServicePath()
	data, err := os.ReadFile(path)
	if err != nil || !serviceDefinitionMatchesRoot(data, p) {
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

func restartSystemdUserService(p *paths.Paths) error {
	_, err := serviceCommandRunner("systemctl", "--user", "restart", systemdServiceName(p))
	if err != nil {
		return fmt.Errorf("systemctl restart: %w", err)
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

func systemdUserServicePath(p *paths.Paths) string {
	home, err := serviceUserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
}

func legacySystemdUserServicePath() string {
	home, _ := serviceUserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", legacySystemdServiceName)
}

// renderSystemdUnit renders the systemd unit, resolving the proxy environment
// from the current process environment itself. It is a convenience wrapper used
// only by tests; production callers use renderSystemdUnitWithProxyEnv, because
// both the install path and drift detection resolve the proxy environment once
// (preferring the on-disk definition when the live environment has none) and
// pass it in.
func renderSystemdUnit(exe string, p *paths.Paths, home string) string {
	return renderSystemdUnitWithProxyEnv(exe, p, home, serviceProxyEnv())
}

// renderSystemdUnitWithProxyEnv renders the systemd unit using a proxy
// environment supplied by the caller (see serviceProxyEnv).
func renderSystemdUnitWithProxyEnv(exe string, p *paths.Paths, home string, proxyEnv [][2]string) string {
	command := strings.Join([]string{
		systemdEscapeArg(exe),
		systemdEscapeArg("daemon"),
		systemdEscapeArg("run"),
		systemdEscapeArg("--root"),
		systemdEscapeArg(p.Root()),
	}, " ")
	envLines := []string{
		systemdEnvironmentLine("HOME", home),
		systemdEnvironmentLine("PATH", managedServicePath(home)),
	}
	// Forward proxy variables so the daemon (and the agents it spawns) can
	// reach the network through the user's proxy. See serviceProxyEnv.
	for _, kv := range proxyEnv {
		envLines = append(envLines, systemdEnvironmentLine(kv[0], kv[1]))
	}
	return fmt.Sprintf(`[Unit]
Description=no-mistakes background daemon

[Service]
Type=simple
ExecStart=%s
WorkingDirectory=%s
%s
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, command, systemdEscapeArg(p.Root()), strings.Join(envLines, "\n"))
}

// systemdEnvironmentLine renders one `Environment=` directive. systemd runs
// specifier expansion on directive values, so a literal `%` (e.g. a
// percent-encoded character in a forwarded proxy credential like
// http://user:p%40ss@proxy:8080) must be doubled to `%%` to survive it -
// otherwise a known specifier letter corrupts the value and an unknown one
// rejects the assignment on systemd >= v249. strconv.Quote never emits `%`, so
// doubling first leaves the quoting unaffected.
func systemdEnvironmentLine(key, value string) string {
	return "Environment=" + strconv.Quote(strings.ReplaceAll(key+"="+value, "%", "%%"))
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

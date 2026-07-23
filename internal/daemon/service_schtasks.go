package daemon

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// installWindowsTask registers the daemon as a per-user scheduled task.
//
// Unlike the launchd and systemd paths, it deliberately does not forward proxy
// environment variables (see serviceProxyEnv). A schtasks /SC ONLOGON task runs
// in the user's interactive logon session and inherits that session's
// environment, so the user's HTTP(S)_PROXY/NO_PROXY/etc. are already present
// without baking them into the task definition. That also means no proxy URL -
// which can embed credentials - is ever written to disk here, so the 0600
// tightening that writeServiceFile applies to the launchd/systemd files has no
// Windows equivalent to worry about.
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
	cleanupLegacyWindowsTask(p)
	return nil
}

func cleanupLegacyWindowsTask(p *paths.Paths) {
	data, err := serviceCommandRunner("schtasks", "/Query", "/TN", legacyWindowsTaskName, "/XML")
	if err != nil || !serviceDefinitionMatchesRoot(data, p) {
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

type windowsManagedDaemonObservation struct {
	state         int
	runGeneration string
}

func inspectWindowsManagedDaemon(p *paths.Paths) (windowsManagedDaemonObservation, error) {
	taskName := strings.ReplaceAll(windowsTaskName(p), "'", "''")
	command := "$task=Get-ScheduledTask -TaskName '" + taskName + "';$info=Get-ScheduledTaskInfo -TaskName '" + taskName + "';Write-Output \"$([int]$task.State)|$($info.LastRunTime.Ticks)|$($info.LastTaskResult)\""
	output, err := serviceCommandRunner(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		command,
	)
	if err != nil {
		return windowsManagedDaemonObservation{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "|")
	if len(fields) != 3 {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task state: unexpected field count %d", len(fields))
	}
	state, err := strconv.Atoi(fields[0])
	if err != nil {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task state: %w", err)
	}
	if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task last run: %w", err)
	}
	if _, err := strconv.ParseUint(fields[2], 10, 32); err != nil {
		return windowsManagedDaemonObservation{}, fmt.Errorf("parse scheduled task result: %w", err)
	}
	return windowsManagedDaemonObservation{state: state, runGeneration: fields[1]}, nil
}

func windowsManagedDaemonState(p *paths.Paths, launch managedServiceLaunch) (managedServiceState, error) {
	observation, err := inspectWindowsManagedDaemon(p)
	if err != nil {
		return managedServiceUnknown, err
	}
	switch observation.state {
	case 4:
		return managedServiceRunning, nil
	case 1:
		return managedServiceExited, nil
	case 3:
		if launch.windowsRunGeneration != "" && observation.runGeneration != launch.windowsRunGeneration {
			return managedServiceExited, nil
		}
		return managedServiceUnknown, nil
	default:
		return managedServiceUnknown, nil
	}
}

func buildWindowsTaskCommand(exe, root string) string {
	args := []string{quoteWindowsTaskArg(exe), "daemon", "run", "--root", quoteWindowsTaskArg(root)}
	return strings.Join(args, " ")
}

func quoteWindowsTaskArg(arg string) string {
	if !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	return strconv.Quote(arg)
}

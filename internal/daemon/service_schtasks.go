package daemon

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

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

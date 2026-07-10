//go:build windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// shimTargetPattern matches the quoted native executable an npm .cmd shim
// launches. npm's generated launcher runs the wrapped binary via a line like:
//
//	"%dp0%\node_modules\@anthropic-ai\claude-code\bin\claude.exe"   %*
//
// The path is relative to the shim's own directory (%~dp0, sometimes captured
// into a %dp0% variable first), so the capture group holds the portion after
// that prefix. Matching is case-insensitive because .cmd shims are.
var shimTargetPattern = regexp.MustCompile(`(?i)"%~?dp0%?\\(.+?\.exe)"`)

// resolveAgentBinary returns the native executable a Windows npm .cmd/.bat shim
// wraps, so exec can launch it directly instead of through cmd.exe. cmd.exe's
// %* forwarding corrupts a multi-line -p prompt and, in a console-less daemon,
// never delivers stdin to the wrapped process (issue #427). It is best-effort:
// any failure to locate or parse a shim falls back to the original bin, which
// also covers non-npm installs where bin is already a native executable.
func resolveAgentBinary(bin string) string {
	path, err := exec.LookPath(bin)
	if err != nil {
		return bin
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".cmd" && ext != ".bat" {
		return bin
	}
	if target := shimNativeTarget(path); target != "" {
		return target
	}
	return bin
}

// shimNativeTarget reads an npm .cmd/.bat shim and returns the absolute path to
// the native executable it launches, or "" if the file is not a recognizable
// shim or the target does not exist on disk. The captured path is relative to
// the shim's own directory (%~dp0).
func shimNativeTarget(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := shimTargetPattern.FindSubmatch(content)
	if m == nil {
		return ""
	}
	target := filepath.Join(filepath.Dir(path), string(m[1]))
	if _, err := os.Stat(target); err != nil {
		return ""
	}
	return filepath.Clean(target)
}

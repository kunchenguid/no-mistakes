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
// that prefix. Matching is case-insensitive because .cmd shims are. The
// trailing %* is required so the match anchors to the real launch line: npm's
// node-launcher shim form for a JS bin opens with an `IF EXIST "%dp0%\node.exe"`
// guard whose quoted .exe has no %* after it, so that form correctly matches
// nothing here and falls back to the shim rather than execing node.exe.
var shimTargetPattern = regexp.MustCompile(`(?i)"%~?dp0%?\\(.+?\.exe)"\s*%\*`)

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
//
// The resolved target must live under a node_modules directory. npm installs a
// package's binary there whether the launcher sits in a project's
// node_modules/.bin or the global prefix, so this both confirms the shim is an
// npm-generated launcher and scopes the bypass to npm-managed executables. A
// hand-written .cmd wrapper that performs its own setup before launching an
// unrelated .exe would resolve outside node_modules and is left alone, so exec
// runs the wrapper and its setup rather than skipping straight to the .exe.
func shimNativeTarget(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := shimTargetPattern.FindSubmatch(content)
	if m == nil {
		return ""
	}
	target := filepath.Clean(filepath.Join(filepath.Dir(path), string(m[1])))
	if !underNodeModules(target) {
		return ""
	}
	if _, err := os.Stat(target); err != nil {
		return ""
	}
	return target
}

// underNodeModules reports whether path has a "node_modules" path segment,
// matched case-insensitively because Windows paths are. It checks whole
// segments so an unrelated directory like "my_node_modules_backup" does not
// qualify.
func underNodeModules(path string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.EqualFold(seg, "node_modules") {
			return true
		}
	}
	return false
}

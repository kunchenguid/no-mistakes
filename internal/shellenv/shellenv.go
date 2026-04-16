package shellenv

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var runtimeGOOS = runtime.GOOS
var lookupEnv = os.LookupEnv
var currentUser = user.Current
var shellCommandOutput = defaultShellCommandOutput

var cacheMu sync.Mutex
var cachedEnv []string

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

func Resolve() ([]string, error) {
	cacheMu.Lock()
	if cachedEnv != nil {
		defer cacheMu.Unlock()
		return append([]string(nil), cachedEnv...), nil
	}
	cacheMu.Unlock()

	resolved, err := resolveUncached()
	if err != nil {
		return nil, err
	}

	cacheMu.Lock()
	if cachedEnv == nil {
		cachedEnv = append([]string(nil), resolved...)
	}
	defer cacheMu.Unlock()
	return append([]string(nil), cachedEnv...), nil
}

func ApplyToProcess() error {
	env, err := Resolve()
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

func resolveUncached() ([]string, error) {
	if runtimeGOOS == "windows" {
		return append([]string(nil), os.Environ()...), nil
	}

	shell := LoginShell()
	args := []string{"-l", "-c", "env -0"}
	if SupportsInteractive(shell) {
		args = []string{"-l", "-i", "-c", "env -0"}
	}
	out, err := shellCommandOutput(shell, args...)
	if err != nil {
		fallback := append([]string(nil), os.Environ()...)
		return ensureShellEntry(fallback, shell), nil
	}
	resolved := parseEnvOutput(out)
	if len(resolved) == 0 {
		fallback := append([]string(nil), os.Environ()...)
		return ensureShellEntry(fallback, shell), nil
	}
	return ensureShellEntry(resolved, shell), nil
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
	cmd := exec.Command(name, args...)
	return cmd.Output()
}

func resetForTests() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cachedEnv = nil
}

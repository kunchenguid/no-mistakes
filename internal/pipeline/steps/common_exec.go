package steps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func envValue(env []string, key string) (string, bool) {
	return envValueForOS(env, key, runtime.GOOS)
}

func envValueForOS(env []string, key, goos string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
		if goos == "windows" && len(entry) >= len(prefix) && strings.EqualFold(entry[:len(prefix)], prefix) {
			return entry[len(prefix):], true
		}
	}
	return "", false
}

func envKey(entry string) string {
	key, _, found := strings.Cut(entry, "=")
	if !found {
		key = entry
	}
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func mergeEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	merged := make([]string, 0, len(os.Environ())+len(extra))
	overrides := make(map[string]string, len(extra))
	for _, entry := range extra {
		overrides[envKey(entry)] = entry
	}
	for _, entry := range os.Environ() {
		key := envKey(entry)
		if override, ok := overrides[key]; ok {
			merged = append(merged, override)
			delete(overrides, key)
			continue
		}
		merged = append(merged, entry)
	}
	for _, entry := range extra {
		key := envKey(entry)
		if override, ok := overrides[key]; ok {
			merged = append(merged, override)
			delete(overrides, key)
		}
	}
	return merged
}

func executableCandidates(name string, env []string) []string {
	return executableCandidatesForOS(runtime.GOOS, name, env)
}

func executableCandidatesForOS(goos, name string, env []string) []string {
	candidates := []string{name}
	if goos != "windows" || filepath.Ext(name) != "" {
		return candidates
	}
	pathExt := ".COM;.EXE;.BAT;.CMD"
	if customPathExt, ok := envValueForOS(env, "PATHEXT", goos); ok {
		pathExt = customPathExt
	}
	for _, ext := range strings.Split(pathExt, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		candidates = append(candidates, name+ext)
	}
	return candidates
}

func findInCustomPath(workDir string, env []string, name string) string {
	customPath, ok := envValue(env, "PATH")
	if !ok {
		return ""
	}
	for _, dir := range filepath.SplitList(customPath) {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workDir, dir)
		}
		for _, candidateName := range executableCandidates(name, env) {
			candidate := filepath.Join(dir, candidateName)
			if fi, err := os.Stat(candidate); err == nil && pathCandidateUsable(runtime.GOOS, candidate, fi) {
				return candidate
			}
		}
	}
	return ""
}

func pathCandidateUsable(goos, path string, fi os.FileInfo) bool {
	if fi.IsDir() {
		return false
	}
	if goos == "windows" {
		return filepath.Ext(path) != ""
	}
	return fi.Mode().Perm()&0o111 != 0
}

func missingFromCustomPath(env []string, name string) string {
	customPath, ok := envValue(env, "PATH")
	if !ok {
		return ""
	}
	missing := filepath.Join(".", executableCandidates(name, env)[0])
	for _, dir := range filepath.SplitList(customPath) {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		return filepath.Join(dir, executableCandidates(name, env)[0])
	}
	return missing
}

// stepCmd creates an exec.Cmd that inherits the StepContext's extra Env, if any.
// When sctx.Env overrides PATH, the binary is resolved from the overridden PATH
// so that tests can inject fake binaries without modifying the process environment.
func stepCmd(sctx *pipeline.StepContext, name string, args ...string) *exec.Cmd {
	resolved := name
	missingFromPath := false
	if len(sctx.Env) > 0 && !strings.Contains(name, string(filepath.Separator)) {
		if candidate := findInCustomPath(sctx.WorkDir, sctx.Env, name); candidate != "" {
			resolved = candidate
		} else if _, ok := envValue(sctx.Env, "PATH"); ok {
			resolved = missingFromCustomPath(sctx.Env, name)
			missingFromPath = true
		}
	}
	cmd := exec.CommandContext(sctx.Ctx, resolved, args...)
	cmd.Dir = sctx.WorkDir
	if len(sctx.Env) > 0 {
		cmd.Env = mergeEnv(sctx.Env)
	}
	if missingFromPath {
		cmd.Err = &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	return cmd
}

// stepGitRun runs a git command using the StepContext's environment.
// It is like git.Run but respects sctx.Env so that tests can inject a fake git binary.
func stepGitRun(sctx *pipeline.StepContext, args ...string) (string, error) {
	cmd := stepCmd(sctx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

func stepGitHeadSHA(sctx *pipeline.StepContext) (string, error) {
	return stepGitRun(sctx, "rev-parse", "HEAD")
}

func stepGitLsRemote(sctx *pipeline.StepContext, remote, ref string) (string, error) {
	out, err := stepGitRun(sctx, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	parts := strings.Fields(out)
	if len(parts) < 1 {
		return "", nil
	}
	return parts[0], nil
}

func stepGitPush(sctx *pipeline.StepContext, remote, ref, expectedSHA string, forceWithLease bool) error {
	args := []string{"push", remote}
	if forceWithLease {
		if expectedSHA != "" {
			args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA))
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, "HEAD:"+ref)
	_, err := stepGitRun(sctx, args...)
	return err
}

// stepCLIAvailable checks whether the provider CLI binary is available,
// respecting any custom PATH in sctx.Env.
func stepCLIAvailable(sctx *pipeline.StepContext, provider scm.Provider) bool {
	name := provider.CLIName()
	if name == "" {
		return false
	}
	if len(sctx.Env) == 0 {
		return scm.CLIAvailable(provider)
	}
	if candidate := findInCustomPath(sctx.WorkDir, sctx.Env, name); candidate != "" {
		return true
	}
	_, ok := envValue(sctx.Env, "PATH")
	if ok {
		return false
	}
	return scm.CLIAvailable(provider)
}

// stepAuthConfigured checks whether the provider CLI is authenticated,
// using sctx.Env to resolve the binary and pass environment variables.
func stepAuthConfigured(sctx *pipeline.StepContext, provider scm.Provider) bool {
	args := provider.AuthCheckCommand()
	if len(args) == 0 {
		return false
	}
	cmd := stepCmd(sctx, args[0], args[1:]...)
	return cmd.Run() == nil
}

// runShellCommand executes a shell command and returns stdout+stderr, exit code, and error.
// A non-zero exit code is not treated as an error - only exec failures return error.
func runShellCommand(ctx context.Context, dir, cmdStr string) (string, int, error) {
	return runShellCommandWithEnv(ctx, dir, nil, cmdStr)
}

func runStepShellCommand(sctx *pipeline.StepContext, cmdStr string) (string, int, error) {
	return runShellCommandWithEnv(sctx.Ctx, sctx.WorkDir, sctx.Env, cmdStr)
}

func runShellCommandWithEnv(ctx context.Context, dir string, env []string, cmdStr string) (string, int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = mergeEnv(env)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode(), nil
		}
		return "", -1, fmt.Errorf("run command %q: %w", cmdStr, err)
	}
	return string(out), 0, nil
}

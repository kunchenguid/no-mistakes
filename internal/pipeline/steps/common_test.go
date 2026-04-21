package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCopyDirContents_PreservesGitRepo(t *testing.T) {
	t.Parallel()
	ensureGitRepoTemplate(t)

	dir := t.TempDir()
	if err := copyDirContents(gitRepoTemplate.dir, dir); err != nil {
		t.Fatal(err)
	}

	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if headSHA != gitRepoTemplate.headSHA {
		t.Fatalf("HEAD = %q, want %q", headSHA, gitRepoTemplate.headSHA)
	}

	status := gitCmd(t, dir, "status", "--short")
	if status != "" {
		t.Fatalf("expected clean copied repo, got %q", status)
	}

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("stat .git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatalf("stat feature.txt: %v", err)
	}
}

func TestResolveBaseSHA_NonZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := resolveBaseSHA(context.Background(), dir, "abc123", "main")
	if got != "abc123" {
		t.Errorf("resolveBaseSHA non-zero = %q, want abc123", got)
	}
}

func TestResolveBaseSHA_ZeroWithMergeBase(t *testing.T) {
	t.Parallel()
	// Create a repo with main branch and feature branch diverging
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	mainSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feat.txt"), []byte("feat"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature commit")

	zeroSHA := "0000000000000000000000000000000000000000"
	got := resolveBaseSHA(context.Background(), dir, zeroSHA, "main")
	if got != mainSHA {
		t.Errorf("resolveBaseSHA zero with merge-base = %q, want %q", got, mainSHA)
	}
}

func TestResolveBaseSHA_ZeroNoDefaultBranch(t *testing.T) {
	t.Parallel()
	// Repo with no "main" branch — should fall back to empty tree SHA
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("data"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")

	zeroSHA := "0000000000000000000000000000000000000000"
	got := resolveBaseSHA(context.Background(), dir, zeroSHA, "main")
	if got != git.EmptyTreeSHA {
		t.Errorf("resolveBaseSHA zero no default = %q, want %q", got, git.EmptyTreeSHA)
	}
}

func TestResolveDefaultBranchTipSHA_FetchesRemoteTip(t *testing.T) {
	t.Parallel()

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	seed := t.TempDir()
	gitCmd(t, seed, "init")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	gitCmd(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "base")
	gitCmd(t, seed, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "push", "origin", "main")

	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", upstream, ".")
	gitCmd(t, workDir, "checkout", "-b", "feature", "origin/main")

	updater := t.TempDir()
	gitCmd(t, updater, "clone", upstream, ".")
	gitCmd(t, updater, "config", "user.name", "test")
	gitCmd(t, updater, "config", "user.email", "test@test.com")
	gitCmd(t, updater, "checkout", "main")
	if err := os.WriteFile(filepath.Join(updater, "base.txt"), []byte("base updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, updater, "add", "base.txt")
	gitCmd(t, updater, "commit", "-m", "main update")
	remoteTip := gitCmd(t, updater, "rev-parse", "HEAD")
	gitCmd(t, updater, "push", "origin", "main")

	staleOriginTip := gitCmd(t, workDir, "rev-parse", "origin/main")
	if staleOriginTip == remoteTip {
		t.Fatal("expected origin/main to be stale before resolveDefaultBranchTipSHA")
	}

	got := resolveDefaultBranchTipSHA(context.Background(), workDir, upstream, staleOriginTip, "main")
	if got != remoteTip {
		t.Fatalf("resolveDefaultBranchTipSHA = %q, want remote tip %q", got, remoteTip)
	}

	fetchedOriginTip := gitCmd(t, workDir, "rev-parse", "origin/main")
	if fetchedOriginTip != remoteTip {
		t.Fatalf("origin/main after resolve = %q, want %q", fetchedOriginTip, remoteTip)
	}
}

func TestResolveDefaultBranchTipSHA_UsesMatchingRemoteName(t *testing.T) {
	t.Parallel()

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	seed := t.TempDir()
	gitCmd(t, seed, "init")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	gitCmd(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "base")
	gitCmd(t, seed, "remote", "add", "upstream", upstream)
	gitCmd(t, seed, "push", "upstream", "main")

	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", upstream, ".")
	gitCmd(t, workDir, "remote", "rename", "origin", "upstream")
	gitCmd(t, workDir, "checkout", "-b", "feature", "upstream/main")

	updater := t.TempDir()
	gitCmd(t, updater, "clone", upstream, ".")
	gitCmd(t, updater, "config", "user.name", "test")
	gitCmd(t, updater, "config", "user.email", "test@test.com")
	gitCmd(t, updater, "checkout", "main")
	if err := os.WriteFile(filepath.Join(updater, "base.txt"), []byte("base updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, updater, "add", "base.txt")
	gitCmd(t, updater, "commit", "-m", "main update")
	remoteTip := gitCmd(t, updater, "rev-parse", "HEAD")
	gitCmd(t, updater, "push", "origin", "main")

	staleRemoteTip := gitCmd(t, workDir, "rev-parse", "upstream/main")
	if staleRemoteTip == remoteTip {
		t.Fatal("expected upstream/main to be stale before resolveDefaultBranchTipSHA")
	}

	got := resolveDefaultBranchTipSHA(context.Background(), workDir, upstream, staleRemoteTip, "main")
	if got != remoteTip {
		t.Fatalf("resolveDefaultBranchTipSHA = %q, want remote tip %q", got, remoteTip)
	}

	fetchedRemoteTip := gitCmd(t, workDir, "rev-parse", "upstream/main")
	if fetchedRemoteTip != remoteTip {
		t.Fatalf("upstream/main after resolve = %q, want %q", fetchedRemoteTip, remoteTip)
	}
}

func TestResolveDefaultBranchTipSHA_FetchFailureAvoidsStaleOriginRef(t *testing.T) {
	t.Parallel()

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	seed := t.TempDir()
	gitCmd(t, seed, "init")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	gitCmd(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "base")
	gitCmd(t, seed, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "push", "origin", "main")

	workDir := t.TempDir()
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.name", "test")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "remote", "add", "origin", upstream)
	gitCmd(t, workDir, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
	gitCmd(t, workDir, "checkout", "-b", "feature", "origin/main")
	staleOriginTip := gitCmd(t, workDir, "rev-parse", "origin/main")
	gitCmd(t, workDir, "remote", "set-url", "origin", filepath.Join(upstream, "missing"))

	fallbackBaseSHA := "abc123"
	got := resolveDefaultBranchTipSHA(context.Background(), workDir, upstream, fallbackBaseSHA, "main")
	if got != fallbackBaseSHA {
		t.Fatalf("resolveDefaultBranchTipSHA = %q, want fallback base %q when fetch fails", got, fallbackBaseSHA)
	}
	if got == staleOriginTip {
		t.Fatalf("resolveDefaultBranchTipSHA reused stale origin/main %q after fetch failure", got)
	}
}

func TestResolveDefaultBranchTipSHA_FetchFailureAvoidsStaleLocalBranch(t *testing.T) {
	t.Parallel()

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	seed := t.TempDir()
	gitCmd(t, seed, "init")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	gitCmd(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "base")
	gitCmd(t, seed, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "push", "origin", "main")

	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", upstream, ".")
	gitCmd(t, workDir, "checkout", "main")

	updater := t.TempDir()
	gitCmd(t, updater, "clone", upstream, ".")
	gitCmd(t, updater, "config", "user.name", "test")
	gitCmd(t, updater, "config", "user.email", "test@test.com")
	gitCmd(t, updater, "checkout", "main")
	if err := os.WriteFile(filepath.Join(updater, "base.txt"), []byte("base updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, updater, "add", "base.txt")
	gitCmd(t, updater, "commit", "-m", "main update")
	remoteTip := gitCmd(t, updater, "rev-parse", "HEAD")
	gitCmd(t, updater, "push", "origin", "main")

	staleLocalTip := gitCmd(t, workDir, "rev-parse", "main")
	if staleLocalTip == remoteTip {
		t.Fatal("expected local main to be stale before resolveDefaultBranchTipSHA")
	}
	gitCmd(t, workDir, "remote", "set-url", "origin", filepath.Join(upstream, "missing"))

	fallbackBaseSHA := "abc123"
	got := resolveDefaultBranchTipSHA(context.Background(), workDir, upstream, fallbackBaseSHA, "main")
	if got != fallbackBaseSHA {
		t.Fatalf("resolveDefaultBranchTipSHA = %q, want fallback base %q when fetch fails", got, fallbackBaseSHA)
	}
	if got == staleLocalTip {
		t.Fatalf("resolveDefaultBranchTipSHA reused stale local main %q after fetch failure", got)
	}
}

func TestRunShellCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("success", func(t *testing.T) {
		out, code, err := runShellCommand(context.Background(), dir, "echo hello")
		if err != nil {
			t.Fatal(err)
		}
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		if !strings.Contains(out, "hello") {
			t.Errorf("output = %q, want to contain 'hello'", out)
		}
	})

	t.Run("nonzero exit", func(t *testing.T) {
		_, code, err := runShellCommand(context.Background(), dir, "exit 42")
		if err != nil {
			t.Fatal(err)
		}
		if code != 42 {
			t.Errorf("exit code = %d, want 42", code)
		}
	})
}

func TestAllSteps(t *testing.T) {
	t.Parallel()
	steps := AllSteps()
	if len(steps) != 8 {
		t.Fatalf("AllSteps() returned %d steps, want 8", len(steps))
	}
	expected := []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	for i, s := range steps {
		if s.Name() != expected[i] {
			t.Errorf("step %d name = %s, want %s", i, s.Name(), expected[i])
		}
	}
}

func TestStepCLIAvailable_ResolvesExecutableSuffixFromCustomPath(t *testing.T) {
	t.Parallel()

	binDir := fakeCLIBinDir(t)
	logFile := filepath.Join(t.TempDir(), "gh.log")
	linkTestBinary(t, binDir, "gh")

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Env: fakeCLIEnv(binDir, map[string]string{
			"FAKE_CLI_MODE": "gh",
			"FAKE_CLI_LOG":  logFile,
		}),
	}

	if !stepCLIAvailable(sctx, scm.ProviderGitHub) {
		t.Fatal("expected gh to be available from custom PATH")
	}

	cmd := stepCmd(sctx, "gh", "auth", "status")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fake gh: %v", err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "auth status") {
		t.Fatalf("expected fake gh invocation, got %q", string(logData))
	}
}

func TestStepCLIAvailable_IgnoresNonExecutableFromCustomPath(t *testing.T) {
	t.Parallel()

	binDir := t.TempDir()
	shimPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Env:     []string{"PATH=" + binDir},
	}

	if stepCLIAvailable(sctx, scm.ProviderGitHub) {
		t.Fatal("expected non-executable gh to be unavailable from custom PATH")
	}

	cmd := stepCmd(sctx, "gh", "auth", "status")
	if cmd.Path != shimPath {
		t.Fatalf("expected missing custom-path lookup to stay inside %q, got %q", binDir, cmd.Path)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-executable gh to fail to run")
	}
}

func TestPathCandidateUsable_WindowsAcceptsExeWithoutExecBits(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gh.exe")
	if err := os.WriteFile(path, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if !pathCandidateUsable("windows", path, info) {
		t.Fatal("expected .exe without exec bits to be usable on windows")
	}
}

func TestEnvValueForOS_WindowsMatchesEmptyMixedCaseOverride(t *testing.T) {
	t.Parallel()

	value, ok := envValueForOS([]string{"Path=", "PathExt="}, "PATH", "windows")
	if !ok {
		t.Fatal("expected empty mixed-case PATH override to be present")
	}
	if value != "" {
		t.Fatalf("expected empty PATH override, got %q", value)
	}

	value, ok = envValueForOS([]string{"Path=", "PathExt="}, "PATHEXT", "windows")
	if !ok {
		t.Fatal("expected empty mixed-case PATHEXT override to be present")
	}
	if value != "" {
		t.Fatalf("expected empty PATHEXT override, got %q", value)
	}
}

func TestExecutableCandidatesForOS_WindowsHonorsExplicitEmptyPATHEXT(t *testing.T) {
	t.Parallel()

	candidates := executableCandidatesForOS("windows", "gh", []string{"PATHEXT="})
	if len(candidates) != 1 {
		t.Fatalf("expected only bare command name, got %v", candidates)
	}
	if candidates[0] != "gh" {
		t.Fatalf("expected bare command candidate, got %q", candidates[0])
	}
}

func TestStepCmd_ResolvesRelativeCustomPathFromWorkDir(t *testing.T) {
	t.Parallel()

	// Use os.MkdirTemp instead of t.TempDir so we can retry cleanup on
	// Windows where the executed exe may briefly hold a file lock.
	workDir, err := os.MkdirTemp("", "stepCmd-relpath")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(workDir); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "gh.log")
	linkTestBinary(t, binDir, "gh")

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: workDir,
		Env: []string{
			"PATH=bin",
			"FAKE_CLI_MODE=gh",
			"FAKE_CLI_LOG=" + logFile,
		},
	}

	if !stepCLIAvailable(sctx, scm.ProviderGitHub) {
		t.Fatal("expected gh to be available from workdir-relative custom PATH")
	}

	cmd := stepCmd(sctx, "gh", "auth", "status")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fake gh from relative custom PATH: %v", err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "auth status") {
		t.Fatalf("expected fake gh invocation, got %q", string(logData))
	}
}

func TestStepCmd_DoesNotFallbackToHostPathWhenCustomPathOmitsBinary(t *testing.T) {
	t.Parallel()

	customPath := t.TempDir()
	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Env:     []string{"PATH=" + customPath},
	}

	cmd := stepCmd(sctx, "git", "--version")
	if cmd.Path != filepath.Join(customPath, "git") {
		t.Fatalf("expected custom-path lookup to stay inside %q, got %q", customPath, cmd.Path)
	}
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected git lookup to fail when custom PATH omits git")
	}
}

func TestStepCmd_DoesNotFallbackToHostPathWhenCustomPathIsEmpty(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Env:     []string{"PATH="},
	}

	cmd := stepCmd(sctx, "git", "--version")
	if !strings.Contains(cmd.Path, string(filepath.Separator)) {
		t.Fatalf("expected explicit empty PATH to keep lookup out of host PATH, got %q", cmd.Path)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected git lookup to fail when custom PATH is empty")
	}
}

func TestStepCmd_OverridesPathWithoutDuplicateEntries(t *testing.T) {
	t.Parallel()

	customPath := t.TempDir()
	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Env: []string{
			"PATH=" + customPath,
			"STEP_TEST_VAR=custom",
		},
	}

	cmd := stepCmd(sctx, filepath.Join(string(filepath.Separator), "usr", "bin", "env"))

	pathCount := 0
	for _, entry := range cmd.Env {
		switch {
		case strings.HasPrefix(entry, "PATH="):
			pathCount++
			if entry != "PATH="+customPath {
				t.Fatalf("expected overridden PATH, got %q", entry)
			}
		case entry == "STEP_TEST_VAR=custom":
			continue
		}
	}
	if pathCount != 1 {
		t.Fatalf("expected exactly one PATH entry, got %d in %v", pathCount, cmd.Env)
	}
}

func TestCommitAgentFixes_NoChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	originalHeadSHA := sctx.Run.HeadSHA

	err := commitAgentFixes(sctx, types.StepReview, "should not commit", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if sctx.Run.HeadSHA != originalHeadSHA {
		t.Errorf("HeadSHA changed unexpectedly: %s -> %s", originalHeadSHA, sctx.Run.HeadSHA)
	}
}

func TestCommitAgentFixes_UsesFallbackSummary(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	os.WriteFile(filepath.Join(dir, "agent-change.txt"), []byte("change"), 0o644)
	err := commitAgentFixes(sctx, types.StepLint, "", "fallback lint fix")
	if err != nil {
		t.Fatal(err)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): fallback lint fix" {
		t.Errorf("commit message = %q, want fallback-based message", got)
	}
}

func TestMatchIgnorePattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		// No-slash patterns match against basename
		{"pkg/foo.generated.go", "*.generated.go", true},
		{"foo.generated.go", "*.generated.go", true},
		{"deep/nested/bar.generated.go", "*.generated.go", true},
		{"foo.go", "*.generated.go", false},

		// Directory wildcard patterns
		{"vendor/pkg/foo.go", "vendor/**", true},
		{"vendor/foo.go", "vendor/**", true},
		{"vendor", "vendor/**", true},
		{"myvendor/foo.go", "vendor/**", false},
		{"src/vendor/foo.go", "vendor/**", false},

		// Full path patterns
		{"docs/README.md", "docs/*.md", true},
		{"README.md", "docs/*.md", false},

		// No match
		{"main.go", "*.generated.go", false},
		{"internal/app.go", "vendor/**", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s", tt.path, tt.pattern), func(t *testing.T) {
			got := matchIgnorePattern(tt.path, tt.pattern)
			if got != tt.want {
				t.Errorf("matchIgnorePattern(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestFilterDiff_Empty(t *testing.T) {
	t.Parallel()
	// No patterns → unchanged
	diff := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n+line\n"
	got := filterDiff(diff, nil)
	if got != diff {
		t.Errorf("expected diff unchanged with nil patterns")
	}

	// Empty diff → empty
	got = filterDiff("", []string{"*.go"})
	if got != "" {
		t.Errorf("expected empty output for empty diff")
	}
}

func TestFilterDiff_SingleFile(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/foo.generated.go b/foo.generated.go\n--- a/foo.generated.go\n+++ b/foo.generated.go\n@@ -0,0 +1 @@\n+generated\n"
	got := filterDiff(diff, []string{"*.generated.go"})
	// All lines should be filtered
	if strings.Contains(got, "generated") {
		t.Errorf("expected generated file to be filtered out, got: %q", got)
	}
}

func TestFilterDiff_MultipleFiles(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1 +1 @@",
		"+package main",
		"diff --git a/vendor/lib.go b/vendor/lib.go",
		"--- a/vendor/lib.go",
		"+++ b/vendor/lib.go",
		"@@ -0,0 +1 @@",
		"+vendored",
		"diff --git a/internal/app.go b/internal/app.go",
		"--- a/internal/app.go",
		"+++ b/internal/app.go",
		"@@ -1 +1 @@",
		"+app code",
	}, "\n")

	got := filterDiff(diff, []string{"vendor/**"})

	// main.go should be kept
	if !strings.Contains(got, "main.go") {
		t.Error("expected main.go to remain in diff")
	}
	// vendor/lib.go should be filtered
	if strings.Contains(got, "vendor/lib.go") {
		t.Error("expected vendor/lib.go to be filtered out")
	}
	// internal/app.go should be kept
	if !strings.Contains(got, "internal/app.go") {
		t.Error("expected internal/app.go to remain in diff")
	}
}

func TestFilterDiff_MultiplePatterns(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"+++ b/main.go",
		"+keep",
		"diff --git a/generated.pb.go b/generated.pb.go",
		"+++ b/generated.pb.go",
		"+proto",
		"diff --git a/vendor/dep.go b/vendor/dep.go",
		"+++ b/vendor/dep.go",
		"+dep",
	}, "\n")

	got := filterDiff(diff, []string{"*.pb.go", "vendor/**"})

	if !strings.Contains(got, "main.go") {
		t.Error("expected main.go to remain")
	}
	if strings.Contains(got, "generated.pb.go") {
		t.Error("expected generated.pb.go to be filtered")
	}
	if strings.Contains(got, "vendor/dep.go") {
		t.Error("expected vendor/dep.go to be filtered")
	}
}

func TestFilterDiff_PathContainingBDividerSequence(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/a b/c.go b/a b/c.go",
		"--- a/a b/c.go",
		"+++ b/a b/c.go",
		"@@ -1 +1 @@",
		"+generated change",
		"diff --git a/keep.go b/keep.go",
		"--- a/keep.go",
		"+++ b/keep.go",
		"@@ -1 +1 @@",
		"+keep change",
	}, "\n")

	got := filterDiff(diff, []string{"*.go"})

	if strings.Contains(got, "a b/c.go") {
		t.Fatalf("expected path containing ' b/' to be filtered via full diff path parsing, got: %q", got)
	}
	if strings.Contains(got, "keep.go") {
		t.Fatalf("expected keep.go to be filtered by basename ignore pattern, got: %q", got)
	}

	got = filterDiff(diff, []string{"a b/c.go"})

	if strings.Contains(got, "a b/c.go") {
		t.Fatalf("expected exact path ignore pattern to filter file with embedded ' b/', got: %q", got)
	}
	if !strings.Contains(got, "keep.go") {
		t.Fatalf("expected keep.go to remain when only embedded-' b/' path is ignored, got: %q", got)
	}
}

func TestReviewFindingsSchema_ValidJSON(t *testing.T) {
	t.Parallel()
	if !json.Valid(reviewFindingsSchema) {
		t.Errorf("reviewFindingsSchema is not valid JSON: %s", string(reviewFindingsSchema))
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(reviewFindingsSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	required, ok := parsed["required"].([]interface{})
	if !ok {
		t.Fatal("expected 'required' array in schema")
	}
	want := map[string]bool{"findings": false, "risk_level": false, "risk_rationale": false}
	for _, r := range required {
		s, _ := r.(string)
		want[s] = true
	}
	for field, found := range want {
		if !found {
			t.Errorf("missing required field %q in schema", field)
		}
	}
}

func TestFindingsSchema_Action(t *testing.T) {
	t.Parallel()
	var parsed map[string]interface{}
	if err := json.Unmarshal(findingsSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["properties"].(map[string]interface{})
	items := props["findings"].(map[string]interface{})["items"].(map[string]interface{})
	itemProps := items["properties"].(map[string]interface{})
	if _, ok := itemProps["action"]; !ok {
		t.Error("findingsSchema missing action property")
	}
	required := items["required"].([]interface{})
	found := false
	for _, r := range required {
		if r.(string) == "action" {
			found = true
		}
	}
	if !found {
		t.Error("findingsSchema does not require action at item level")
	}
}

func TestFindingsSchema_IncludesTestedArray(t *testing.T) {
	t.Parallel()
	var parsed map[string]interface{}
	if err := json.Unmarshal(findingsSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["properties"].(map[string]interface{})
	tested, ok := props["tested"].(map[string]interface{})
	if !ok {
		t.Fatal("findingsSchema missing tested property")
	}
	if tested["type"] != "array" {
		t.Fatalf("expected tested to be an array, got %#v", tested["type"])
	}
	if tested["items"].(map[string]interface{})["type"] != "string" {
		t.Fatalf("expected tested items to be strings, got %#v", tested["items"])
	}
}

func TestFindingsSchema_IncludesTestingSummary(t *testing.T) {
	t.Parallel()
	var parsed map[string]interface{}
	if err := json.Unmarshal(findingsSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["properties"].(map[string]interface{})
	field, ok := props["testing_summary"].(map[string]interface{})
	if !ok {
		t.Fatal("findingsSchema missing testing_summary property")
	}
	if field["type"] != "string" {
		t.Fatalf("expected testing_summary to be a string, got %#v", field["type"])
	}
}

func TestReviewFindingsSchema_ActionAtItemLevel(t *testing.T) {
	t.Parallel()
	var parsed map[string]interface{}
	if err := json.Unmarshal(reviewFindingsSchema, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["properties"].(map[string]interface{})
	items := props["findings"].(map[string]interface{})["items"].(map[string]interface{})
	required := items["required"].([]interface{})
	found := false
	for _, r := range required {
		if r.(string) == "action" {
			found = true
		}
	}
	if !found {
		t.Error("reviewFindingsSchema does not require action at item level")
	}
}

func TestSanitizedPreviousFindingsForPrompt_PreservesMultilineDescriptions(t *testing.T) {
	t.Parallel()
	raw, err := types.MarshalFindingsJSON(types.Findings{
		Items: []types.Finding{{
			ID:          "review-1",
			Severity:    "warning",
			File:        "internal/pipeline/steps/review.go",
			Line:        278,
			Description: "go test failed:\n--- FAIL: TestThing\nmain_test.go:42: expected x\n",
			Action:      types.ActionAutoFix,
		}},
		Summary:       "1 selected finding",
		RiskLevel:     "medium",
		RiskRationale: "compiler output:\nline 1\nline 2",
	})
	if err != nil {
		t.Fatal(err)
	}

	sanitized := sanitizedPreviousFindingsForPrompt(raw)
	findings, err := types.ParseFindingsJSON(sanitized)
	if err != nil {
		t.Fatalf("ParseFindingsJSON() error = %v", err)
	}
	if !strings.Contains(findings.Items[0].Description, "\n--- FAIL: TestThing\n") {
		t.Fatalf("expected multiline description to be preserved, got %q", findings.Items[0].Description)
	}
	if !strings.Contains(findings.RiskRationale, "\nline 1\nline 2") {
		t.Fatalf("expected multiline risk rationale to be preserved, got %q", findings.RiskRationale)
	}
}

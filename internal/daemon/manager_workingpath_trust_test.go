package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// trustLogCapture keeps the captured log safe from any background goroutine
// that logs through the default logger while it is swapped out.
type trustLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *trustLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *trustLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureWarnings(t *testing.T) *trustLogCapture {
	t.Helper()
	logged := &trustLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logged
}

func trustedDefaultBranchConfig() *config.RepoConfig {
	return &config.RepoConfig{Commands: config.Commands{Lint: "trusted-lint", Test: "trusted-test"}}
}

func writeWorkingPathConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestApplyWorkingPathTrustedConfig_ParseFailureKeepsTrustedCopy pins the
// trust-boundary fail-safe: a malformed working-path file must never swap the
// maintainer's default-branch commands for empty ones, and the substitution
// must be visible in the log rather than silent.
func TestApplyWorkingPathTrustedConfig_ParseFailureKeepsTrustedCopy(t *testing.T) {
	workingPath := t.TempDir()
	writeWorkingPathConfig(t, workingPath, "commands:\n  lint: [unclosed\n")

	logged := captureWarnings(t)
	globalCfg := &config.GlobalConfig{TrustWorkingPathConfig: true}
	repo := &db.Repo{WorkingPath: workingPath}
	trusted := trustedDefaultBranchConfig()

	got := applyWorkingPathTrustedConfig(context.Background(), globalCfg, repo, trusted, "run")
	if got != trusted {
		t.Fatalf("expected the trusted copy back unchanged, got %+v", got)
	}
	if got.Commands.Lint != "trusted-lint" {
		t.Fatalf("commands.lint = %q, want %q", got.Commands.Lint, "trusted-lint")
	}
	if out := logged.String(); !strings.Contains(out, "working-path repo config: parse failed") {
		t.Fatalf("expected a warning naming the parse failure, got: %q", out)
	}
}

// TestApplyWorkingPathTrustedConfig_UnstatableFileKeepsTrustedCopy covers the
// non-ErrNotExist stat branch: the opted-in file may be there and unreadable,
// which must not silently drop the maintainer's commands. The premise is an
// ENOTDIR stat error, which Windows reports as fs.ErrNotExist instead, taking
// the ordinary absent-file branch and proving nothing there.
func TestApplyWorkingPathTrustedConfig_UnstatableFileKeepsTrustedCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows maps a non-directory path component to fs.ErrNotExist, which is the absent-file branch")
	}
	base := t.TempDir()
	notADir := filepath.Join(base, "notadir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	logged := captureWarnings(t)
	globalCfg := &config.GlobalConfig{TrustWorkingPathConfig: true}
	repo := &db.Repo{WorkingPath: notADir}
	trusted := trustedDefaultBranchConfig()

	got := applyWorkingPathTrustedConfig(context.Background(), globalCfg, repo, trusted, "run")
	if got != trusted {
		t.Fatalf("expected the trusted copy back unchanged, got %+v", got)
	}
	if out := logged.String(); !strings.Contains(out, "working-path repo config: could not be inspected") {
		t.Fatalf("expected a warning naming the failed inspection, got: %q", out)
	}
}

// TestApplyWorkingPathTrustedConfig_AbsentFileIsSilent is the other half of the
// stat split: an absent file is the ordinary case and says nothing, so it must
// keep the trusted copy WITHOUT a warning. Collapse the split and this fails.
func TestApplyWorkingPathTrustedConfig_AbsentFileIsSilent(t *testing.T) {
	workingPath := t.TempDir()

	logged := captureWarnings(t)
	globalCfg := &config.GlobalConfig{TrustWorkingPathConfig: true}
	repo := &db.Repo{WorkingPath: workingPath}
	trusted := trustedDefaultBranchConfig()

	got := applyWorkingPathTrustedConfig(context.Background(), globalCfg, repo, trusted, "run")
	if got != trusted {
		t.Fatalf("expected the trusted copy back unchanged, got %+v", got)
	}
	if out := logged.String(); out != "" {
		t.Fatalf("an absent working-path config must not warn, got: %q", out)
	}
}

// TestApplyWorkingPathTrustedConfig_TrackedFileStillApplies pins the deliberate
// warn-only decision: a git-tracked working-path config is a footgun but the
// maintainer opted in, so it still steers the run.
func TestApplyWorkingPathTrustedConfig_TrackedFileStillApplies(t *testing.T) {
	workingPath := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := git.Run(ctx, workingPath, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	writeWorkingPathConfig(t, workingPath, "commands:\n  lint: working-lint\n")
	if _, err := git.Run(ctx, workingPath, "add", ".no-mistakes.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, workingPath, "commit", "-m", "add config"); err != nil {
		t.Fatal(err)
	}

	logged := captureWarnings(t)
	globalCfg := &config.GlobalConfig{TrustWorkingPathConfig: true}
	repo := &db.Repo{WorkingPath: workingPath}
	trusted := trustedDefaultBranchConfig()

	got := applyWorkingPathTrustedConfig(ctx, globalCfg, repo, trusted, "run")
	if out := logged.String(); !strings.Contains(out, "working-path repo config is tracked by git") {
		t.Fatalf("expected a warning naming the tracked file, got: %q", out)
	}
	if got.Commands.Lint != "working-lint" {
		t.Fatalf("commands.lint = %q, want the tracked working-path value", got.Commands.Lint)
	}
	if got.Commands.Test != "trusted-test" {
		t.Fatalf("commands.test = %q, want the trusted default-branch value", got.Commands.Test)
	}
}

// TestApplyWorkingPathTrustedConfig_OptInOffIgnoresWorkingPath keeps the opt-in
// itself honest at the unit level.
func TestApplyWorkingPathTrustedConfig_OptInOffIgnoresWorkingPath(t *testing.T) {
	workingPath := t.TempDir()
	writeWorkingPathConfig(t, workingPath, "commands:\n  lint: working-lint\n")

	globalCfg := &config.GlobalConfig{}
	repo := &db.Repo{WorkingPath: workingPath}
	trusted := trustedDefaultBranchConfig()

	got := applyWorkingPathTrustedConfig(context.Background(), globalCfg, repo, trusted, "run")
	if got.Commands.Lint != "trusted-lint" {
		t.Fatalf("commands.lint = %q, want the trusted value while the opt-in is off", got.Commands.Lint)
	}
}

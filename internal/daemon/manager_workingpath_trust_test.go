package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

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
// maintainer's default-branch commands for empty ones.
func TestApplyWorkingPathTrustedConfig_ParseFailureKeepsTrustedCopy(t *testing.T) {
	workingPath := t.TempDir()
	writeWorkingPathConfig(t, workingPath, "commands:\n  lint: [unclosed\n")

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
}

// TestApplyWorkingPathTrustedConfig_UnstatableFileKeepsTrustedCopy covers the
// non-ErrNotExist stat branch: the opted-in file may be there and unreadable,
// which must not silently drop the maintainer's commands.
func TestApplyWorkingPathTrustedConfig_UnstatableFileKeepsTrustedCopy(t *testing.T) {
	base := t.TempDir()
	notADir := filepath.Join(base, "notadir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	globalCfg := &config.GlobalConfig{TrustWorkingPathConfig: true}
	repo := &db.Repo{WorkingPath: notADir}
	trusted := trustedDefaultBranchConfig()

	got := applyWorkingPathTrustedConfig(context.Background(), globalCfg, repo, trusted, "run")
	if got != trusted {
		t.Fatalf("expected the trusted copy back unchanged, got %+v", got)
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

	globalCfg := &config.GlobalConfig{TrustWorkingPathConfig: true}
	repo := &db.Repo{WorkingPath: workingPath}
	trusted := trustedDefaultBranchConfig()

	got := applyWorkingPathTrustedConfig(ctx, globalCfg, repo, trusted, "run")
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

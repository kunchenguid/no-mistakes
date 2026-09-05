package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// TestTestCommandSufficient_PushedBranchCannotSelfDeclare drives the REAL
// trusted-config path - a real git repository, a real default-branch fetch, the
// daemon's own loadTrustedRepoConfig at a pinned SHA, and config.LoadRepo on the
// checked-out pushed head - rather than hand-built RepoConfig values. It is the
// end-to-end answer to "can a contributor's branch declare its own test
// sufficient and skip the evidence gate that reviews it?".
//
// The default branch carries no declaration. The pushed branch carries both the
// declaration and a trivially passing command. The effective config the pipeline
// runs under must contain neither.
func TestTestCommandSufficient_PushedBranchCannotSelfDeclare(t *testing.T) {
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")

	// Trusted default branch: a real test command, but no sufficiency
	// declaration. The maintainer has not said an exit code proves intent.
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  test: \"echo trusted-test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "trusted config")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// The contributor's pushed branch tries to declare its own test sufficient
	// alongside a command that proves nothing.
	gitCmd(t, src, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("test_command_sufficient: true\ncommands:\n  test: \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "self-declare test sufficient")
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/feature")
	pushedSHA := gitOutput(t, src, "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	if err := git.WorktreeAdd(ctx, bare, wt, pushedSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("fetch main: %v", err)
	}
	trustedSHA, err := git.ResolveRef(ctx, wt, "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}

	// Sanity: the hostile declaration really is present on the checked-out
	// pushed head, so a false negative below would be a real result.
	pushed, err := config.LoadRepo(wt)
	if err != nil {
		t.Fatalf("load pushed repo config: %v", err)
	}
	if !pushed.TestCommandSufficient {
		t.Fatal("expected the pushed branch's declaration to parse; the assertion below would otherwise be vacuous")
	}
	if pushed.Commands.Test != "true" {
		t.Fatalf("pushed commands.test = %q, want the contributor's own command", pushed.Commands.Test)
	}

	trusted := loadTrustedRepoConfig(ctx, wt, trustedSHA, "test-run")
	if trusted == nil {
		t.Fatal("expected the trusted default-branch config to load")
	}

	effective := config.EffectiveRepoConfig(pushed, trusted, false)
	if effective.TestCommandSufficient {
		t.Fatal("SECURITY REGRESSION: a pushed branch self-declared its test sufficient; the declaration must come from the trusted default branch only")
	}
	if effective.Commands.Test != "echo trusted-test" {
		t.Fatalf("effective commands.test = %q, want the trusted default-branch command", effective.Commands.Test)
	}

	// And the resolved pipeline config the Test step actually reads carries the
	// same answer, so nothing downstream re-introduces the pushed value.
	merged := config.Merge(&config.GlobalConfig{}, effective)
	if merged.TestCommandSufficient {
		t.Fatal("SECURITY REGRESSION: the pushed declaration reached the resolved pipeline config")
	}
}

// TestTestCommandSufficient_TrustedDeclarationIsHonored is the paired
// positive case, so the assertion above is known to guard a real mechanism
// rather than a field nothing ever sets. Same real git plumbing, with the
// maintainer's declaration on the default branch where it belongs.
func TestTestCommandSufficient_TrustedDeclarationIsHonored(t *testing.T) {
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("test_command_sufficient: true\ncommands:\n  test: \"echo trusted-test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "trusted config with declaration")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// A contributor branch that changes code and leaves the config alone.
	gitCmd(t, src, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(src, "app.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "ordinary change")
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/feature")
	pushedSHA := gitOutput(t, src, "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	if err := git.WorktreeAdd(ctx, bare, wt, pushedSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("fetch main: %v", err)
	}
	trustedSHA, err := git.ResolveRef(ctx, wt, "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}

	pushed, err := config.LoadRepo(wt)
	if err != nil {
		t.Fatalf("load pushed repo config: %v", err)
	}
	trusted := loadTrustedRepoConfig(ctx, wt, trustedSHA, "test-run")
	if trusted == nil {
		t.Fatal("expected the trusted default-branch config to load")
	}

	merged := config.Merge(&config.GlobalConfig{}, config.EffectiveRepoConfig(pushed, trusted, false))
	if !merged.TestCommandSufficient {
		t.Fatal("expected the maintainer's trusted declaration to reach the resolved pipeline config")
	}
	if merged.Commands.Test != "echo trusted-test" {
		t.Fatalf("resolved commands.test = %q, want the trusted command", merged.Commands.Test)
	}
}

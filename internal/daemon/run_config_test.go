package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestLoadSubmittedRepoConfigUsesImmutableSubmittedHead(t *testing.T) {
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init", "--initial-branch=main")
	gitCmd(t, repoDir, "config", "user.email", "test@test.com")
	gitCmd(t, repoDir, "config", "user.name", "Test")
	gitCmd(t, repoDir, "config", "commit.gpgsign", "false")

	configPath := filepath.Join(repoDir, ".no-mistakes.yaml")
	if err := os.WriteFile(configPath, []byte("commands:\n  test: candidate-sentinel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".no-mistakes.yaml")
	gitCmd(t, repoDir, "commit", "-m", "candidate config")
	submittedSHA := gitOutput(t, repoDir, "rev-parse", "HEAD")

	if err := os.WriteFile(configPath, []byte("commands:\n  test: changed-after-run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".no-mistakes.yaml")
	gitCmd(t, repoDir, "commit", "-m", "later config")
	currentSHA := gitOutput(t, repoDir, "rev-parse", "HEAD")

	run := &db.Run{HeadSHA: currentSHA, SubmittedHeadSHA: &submittedSHA}
	got, err := loadSubmittedRepoConfig(context.Background(), repoDir, run)
	if err != nil {
		t.Fatalf("load submitted config: %v", err)
	}
	if got.Commands.Test != "candidate-sentinel" {
		t.Fatalf("commands.test = %q, want immutable candidate-sentinel", got.Commands.Test)
	}

	// Legacy rows have no submitted_head_sha. Their only compatible recovery
	// source is the head_sha they retained.
	legacy := &db.Run{HeadSHA: submittedSHA}
	got, err = loadSubmittedRepoConfig(context.Background(), repoDir, legacy)
	if err != nil {
		t.Fatalf("load legacy config: %v", err)
	}
	if got.Commands.Test != "candidate-sentinel" {
		t.Fatalf("legacy commands.test = %q, want head snapshot", got.Commands.Test)
	}
}

func TestLoadSubmittedRepoConfigAbsentCandidateDoesNotBorrowLaterConfig(t *testing.T) {
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init", "--initial-branch=main")
	gitCmd(t, repoDir, "config", "user.email", "test@test.com")
	gitCmd(t, repoDir, "config", "user.name", "Test")
	gitCmd(t, repoDir, "config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", "README.md")
	gitCmd(t, repoDir, "commit", "-m", "candidate without config")
	submittedSHA := gitOutput(t, repoDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repoDir, ".no-mistakes.yaml"), []byte("commands:\n  test: unrelated-later-command\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".no-mistakes.yaml")
	gitCmd(t, repoDir, "commit", "-m", "later unrelated config")
	currentSHA := gitOutput(t, repoDir, "rev-parse", "HEAD")

	got, err := loadSubmittedRepoConfig(context.Background(), repoDir, &db.Run{
		HeadSHA:          currentSHA,
		SubmittedHeadSHA: &submittedSHA,
	})
	if err != nil {
		t.Fatalf("load absent candidate config: %v", err)
	}
	if got.Commands.Test != "" || got.Commands.Lint != "" || got.Commands.Format != "" {
		t.Fatalf("absent candidate borrowed commands: %#v", got.Commands)
	}
}

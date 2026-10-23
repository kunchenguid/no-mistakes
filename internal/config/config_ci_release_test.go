package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCIReleaseAfterChecksPassDefaultsOffAndResolvesFromTrustedConfig is the
// regression for the CI step's opt-in release verdict.
//
// The default is what the step has always done: monitor the pull request until
// it is merged or closed. Finishing at the checks verdict is an explicit opt-in,
// never an assumed consent (VISION.md), and a trusted repository config may set
// it for its own workflow while a pushed branch cannot.
func TestCIReleaseAfterChecksPassDefaultsOffAndResolvesFromTrustedConfig(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, ".no-mistakes.yaml")
	if err := os.WriteFile(repoPath, []byte("ci:\n  release_after_checks_pass: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("repo yaml with the ci block must parse: %v", err)
	}
	if repo.CI.ReleaseAfterChecksPass == nil || !*repo.CI.ReleaseAfterChecksPass {
		t.Fatalf("repo ci.release_after_checks_pass = %v, want true", repo.CI.ReleaseAfterChecksPass)
	}

	globalPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(globalPath, []byte("agent: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobal(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	if merged := Merge(global, &RepoConfig{}); merged.CI.ReleaseAfterChecksPass {
		t.Fatal("an unset ci.release_after_checks_pass must keep the monitor, not release the run")
	}
	if merged := Merge(global, repo); !merged.CI.ReleaseAfterChecksPass {
		t.Fatal("the repository's trusted opt-in did not reach the merged config the CI step reads")
	}

	// The global ci block is the operator's floor, and a repository may opt back
	// out in both directions.
	if err := os.WriteFile(globalPath, []byte("ci:\n  release_after_checks_pass: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err = LoadGlobal(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	if global.CI.ReleaseAfterChecksPass == nil || !*global.CI.ReleaseAfterChecksPass {
		t.Fatal("ci.release_after_checks_pass: true was not honored in the global block")
	}
	if merged := Merge(global, &RepoConfig{}); !merged.CI.ReleaseAfterChecksPass {
		t.Fatal("the global opt-in did not reach the merged config")
	}
	if err := os.WriteFile(repoPath, []byte("ci:\n  release_after_checks_pass: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	optingOut, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if merged := Merge(global, optingOut); merged.CI.ReleaseAfterChecksPass {
		t.Fatal("an explicit repository false must override the global opt-in")
	}
}

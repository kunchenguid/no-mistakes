package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCIMonitorUntilMergedDefaultsOffAndIsGlobalOnly is the trust-boundary and
// default regression for the CI step's release-at-green verdict.
//
// The zero value is the release, so an operator who never sets the key gets the
// behavior the CI step documents; the opt-in watch is an explicit true. RepoConfig
// has no matching field, so a pushed branch can never hold another machine's runs
// open waiting for a merge.
func TestCIMonitorUntilMergedDefaultsOffAndIsGlobalOnly(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, ".no-mistakes.yaml")
	if err := os.WriteFile(repoPath, []byte("ci_monitor_until_merged: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("repo yaml with the global-only key must parse and be ignored: %v", err)
	}
	if repo.Agent != "" || repo.Commands.Test != "" || repo.Commands.Lint != "" {
		t.Fatalf("unrelated repo config fields changed: %#v", repo)
	}

	globalPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(globalPath, []byte("agent: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobal(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	if global.CIMonitorUntilMerged {
		t.Fatal("an unset ci_monitor_until_merged must default to the release, not to the watch")
	}
	if merged := Merge(global, repo); merged.CIMonitorUntilMerged {
		t.Fatal("a repository must not be able to opt a run into the merge watch")
	}

	if err := os.WriteFile(globalPath, []byte("ci_monitor_until_merged: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err = LoadGlobal(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !global.CIMonitorUntilMerged {
		t.Fatal("ci_monitor_until_merged: true was not honored")
	}
	if merged := Merge(global, repo); !merged.CIMonitorUntilMerged {
		t.Fatal("the operator's opt-in watch did not reach the merged config the CI step reads")
	}
}

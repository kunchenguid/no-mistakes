package config

import "testing"

// The working-path copy exists for gated repos whose default branch is owned by
// a team that does not run no-mistakes, so there is nowhere trusted to commit
// commands. It layers over the default-branch copy rather than replacing it.

func TestMergeWorkingPathTrusted_NilWorkingPathIsNoOp(t *testing.T) {
	trusted := &RepoConfig{Commands: Commands{Test: "go test ./..."}}
	if got := MergeWorkingPathTrusted(trusted, nil); got != trusted {
		t.Fatalf("MergeWorkingPathTrusted(trusted, nil) = %v, want the trusted config unchanged", got)
	}
}

func TestMergeWorkingPathTrusted_EmptyFileChangesNothing(t *testing.T) {
	trusted := &RepoConfig{Commands: Commands{Test: "go test ./...", Lint: "golangci-lint run"}}
	got := MergeWorkingPathTrusted(trusted, &RepoConfig{})
	if got.Commands != trusted.Commands {
		t.Fatalf("Commands = %+v, want %+v: an empty working-path file must not blank the trusted copy", got.Commands, trusted.Commands)
	}
}

func TestMergeWorkingPathTrusted_MergesFieldByField(t *testing.T) {
	trusted := &RepoConfig{Commands: Commands{Test: "go test ./...", Lint: "golangci-lint run"}}
	workingPath := &RepoConfig{Commands: Commands{Test: "imr verify --integration"}}

	got := MergeWorkingPathTrusted(trusted, workingPath)

	if got.Commands.Test != "imr verify --integration" {
		t.Errorf("Commands.Test = %q, want the working-path value to win", got.Commands.Test)
	}
	if got.Commands.Lint != "golangci-lint run" {
		t.Errorf("Commands.Lint = %q, want the trusted value preserved: the copies compose, they do not replace", got.Commands.Lint)
	}
}

func TestMergeWorkingPathTrusted_DoesNotMergeAllowRepoCommands(t *testing.T) {
	// allow_repo_commands hands commands/agent to whatever a contributor
	// pushed. A local convenience override must not widen that boundary.
	trusted := &RepoConfig{AllowRepoCommands: false}
	workingPath := &RepoConfig{AllowRepoCommands: true}

	if got := MergeWorkingPathTrusted(trusted, workingPath); got.AllowRepoCommands {
		t.Fatal("AllowRepoCommands = true, want false: the working-path copy must not be able to enable pushed-branch commands")
	}
}

func TestMergeWorkingPathTrusted_DisableProjectSettingsTrueWins(t *testing.T) {
	// A plain bool cannot distinguish "set to false" from "absent", so the
	// working-path copy may turn the opt-out on but never off.
	t.Run("turns on", func(t *testing.T) {
		got := MergeWorkingPathTrusted(&RepoConfig{}, &RepoConfig{DisableProjectSettings: true})
		if !got.DisableProjectSettings {
			t.Fatal("DisableProjectSettings = false, want true")
		}
	})
	t.Run("cannot turn off", func(t *testing.T) {
		got := MergeWorkingPathTrusted(&RepoConfig{DisableProjectSettings: true}, &RepoConfig{})
		if !got.DisableProjectSettings {
			t.Fatal("DisableProjectSettings = false, want true: the weakening direction must not be reachable")
		}
	})
}

func TestMergeWorkingPathTrusted_DoesNotMutateTrusted(t *testing.T) {
	trusted := &RepoConfig{Commands: Commands{Test: "go test ./..."}}
	MergeWorkingPathTrusted(trusted, &RepoConfig{Commands: Commands{Test: "imr verify"}})
	if trusted.Commands.Test != "go test ./..." {
		t.Fatalf("trusted.Commands.Test = %q, want it untouched: the merge must not write through to the caller's config", trusted.Commands.Test)
	}
}

package config

import (
	"slices"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

// agent selects which process launches with the maintainer's credentials, so
// each way the working-path copy can change it is pinned: a list replaces the
// trusted list AND re-derives the single Agent from its head, a bare agent
// replaces the single value AND clears a trusted list that would otherwise
// keep winning at resolution, and an absent agent changes neither.
func TestMergeWorkingPathTrusted_AgentSelection(t *testing.T) {
	trustedConfig := func() *RepoConfig {
		return &RepoConfig{
			Agent:  types.AgentClaude,
			Agents: []types.AgentName{types.AgentClaude, types.AgentCodex},
		}
	}

	t.Run("working-path list replaces the trusted list and its head", func(t *testing.T) {
		trusted := trustedConfig()
		workingPath := &RepoConfig{Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}}

		got := MergeWorkingPathTrusted(trusted, workingPath)

		if !slices.Equal(got.Agents, workingPath.Agents) {
			t.Errorf("Agents = %v, want the working-path list %v", got.Agents, workingPath.Agents)
		}
		if got.Agent != types.AgentCodex {
			t.Errorf("Agent = %q, want %q re-derived from the working-path list head", got.Agent, types.AgentCodex)
		}
		if !slices.Equal(trusted.Agents, trustedConfig().Agents) {
			t.Errorf("trusted config mutated: %v", trusted.Agents)
		}
	})

	t.Run("working-path single agent clears the trusted fallback list", func(t *testing.T) {
		got := MergeWorkingPathTrusted(trustedConfig(), &RepoConfig{Agent: types.AgentCodex})

		if got.Agent != types.AgentCodex {
			t.Errorf("Agent = %q, want the working-path value to win", got.Agent)
		}
		if len(got.Agents) != 0 {
			t.Errorf("Agents = %v, want it cleared: a leftover trusted list would outrank the agent the maintainer chose", got.Agents)
		}
	})

	t.Run("absent working-path agent keeps the trusted selection", func(t *testing.T) {
		got := MergeWorkingPathTrusted(trustedConfig(), &RepoConfig{Commands: Commands{Test: "imr verify"}})

		if got.Agent != types.AgentClaude || !slices.Equal(got.Agents, trustedConfig().Agents) {
			t.Errorf("Agent = %q, Agents = %v, want the trusted selection untouched", got.Agent, got.Agents)
		}
	})
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

// review.path_instructions is a maintainer-owned trusted field like
// document.instructions, so a maintainer who cannot commit to the default
// branch must be able to supply it from the working path - otherwise the
// opt-in silently drops half of what it exists to carry.
func TestMergeWorkingPathTrusted_MergesReviewPathInstructions(t *testing.T) {
	trusted := &RepoConfig{Review: ReviewRaw{PathInstructions: []PathInstruction{
		{Path: "internal/**", Instructions: "default-branch rule"},
	}}}
	workingPath := &RepoConfig{Review: ReviewRaw{PathInstructions: []PathInstruction{
		{Path: "internal/db/**", Instructions: "working-path rule"},
	}}}

	got := MergeWorkingPathTrusted(trusted, workingPath)

	if len(got.Review.PathInstructions) != 1 || got.Review.PathInstructions[0].Instructions != "working-path rule" {
		t.Fatalf("Review.PathInstructions = %+v, want the working-path list to replace the trusted one", got.Review.PathInstructions)
	}
	if len(trusted.Review.PathInstructions) != 1 || trusted.Review.PathInstructions[0].Instructions != "default-branch rule" {
		t.Errorf("trusted config mutated: %+v", trusted.Review.PathInstructions)
	}
}

// An absent working-path review block leaves the maintainer's default-branch
// rules in force, exactly like every other merged field.
func TestMergeWorkingPathTrusted_KeepsTrustedReviewWhenWorkingPathHasNone(t *testing.T) {
	trusted := &RepoConfig{Review: ReviewRaw{PathInstructions: []PathInstruction{
		{Path: "internal/**", Instructions: "default-branch rule"},
	}}}

	got := MergeWorkingPathTrusted(trusted, &RepoConfig{})

	if len(got.Review.PathInstructions) != 1 || got.Review.PathInstructions[0].Instructions != "default-branch rule" {
		t.Fatalf("Review.PathInstructions = %+v, want the trusted list preserved", got.Review.PathInstructions)
	}
}

package config

import (
	"fmt"
	"strings"
	"testing"
)

const operatorInstructionsGlobal = `review:
  path_instructions:
    - path: "**/*.vue"
      instructions: Repeated components are driven from a computed.
repository_overrides:
  https://gitlab.example.com/group/app-one.git:
    review:
      path_instructions:
        - path: "**/*.cs"
          instructions: Sync wording is always "sync from <upstream>".
    document:
      instructions: Configuration keys are owned by docs/reference/config.md.
`

func TestMergeForRemote_OperatorInstructionsAddSourcesInOrder(t *testing.T) {
	t.Parallel()

	global, err := LoadGlobalFromBytes([]byte(operatorInstructionsGlobal))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() rejected operator instructions: %v", err)
	}
	repo := &RepoConfig{
		Review:   ReviewRaw{PathInstructions: []PathInstruction{{Path: "docs/**", Instructions: "Prose only."}}},
		Document: DocumentRaw{Instructions: "README owns install steps."},
	}

	cfg := MergeForRemote(global, repo, "git@gitlab.example.com:group/app-one.git")
	var got []string
	for _, source := range cfg.Review.PathInstructionSources() {
		got = append(got, source.Heading+" | "+source.Entries[0].Path)
	}
	want := []string{
		ReviewGlobalPathInstructionsHeading + " | **/*.vue",
		ReviewRepositoryPathInstructionsHeading + " | **/*.cs",
		ReviewPathInstructionsHeading + " | docs/**",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("PathInstructionSources() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if cfg.Document.RepositoryInstructions != "Configuration keys are owned by docs/reference/config.md." {
		t.Fatalf("Document.RepositoryInstructions = %q", cfg.Document.RepositoryInstructions)
	}
	if cfg.Document.Instructions != "README owns install steps." {
		t.Fatalf("Document.Instructions = %q, want the trusted repository policy kept", cfg.Document.Instructions)
	}

	other := MergeForRemote(global, repo, "https://gitlab.example.com/group/app-two.git")
	if len(other.Review.RepositoryPathInstructions) != 0 || other.Document.RepositoryInstructions != "" {
		t.Fatalf("a non-matching remote received another repository's instructions: %+v %+v", other.Review, other.Document)
	}
	if len(other.Review.GlobalPathInstructions) != 1 {
		t.Fatalf("GlobalPathInstructions = %+v, want the global rule for every repository", other.Review.GlobalPathInstructions)
	}
}

func TestMerge_WithoutOperatorInstructionsKeepsOnlyTheTrustedSource(t *testing.T) {
	t.Parallel()

	global, err := LoadGlobalFromBytes([]byte("log_level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo := &RepoConfig{Review: ReviewRaw{PathInstructions: []PathInstruction{{Path: "docs/**", Instructions: "Prose only."}}}}
	sources := MergeForRemote(global, repo, "https://github.com/acme/widget.git").Review.PathInstructionSources()
	if len(sources) != 1 || sources[0].Heading != ReviewPathInstructionsHeading {
		t.Fatalf("PathInstructionSources() = %+v, want only the trusted source", sources)
	}
}

func TestLoadGlobal_OperatorInstructionsAllowOnlyAdditiveGuidance(t *testing.T) {
	t.Parallel()

	for name, source := range map[string]string{
		"global review conversation":    "review:\n  conversation: true\n",
		"global document instructions":  "document:\n  instructions: x\n",
		"override review conversation":  "repository_overrides:\n  https://github.com/acme/widget.git:\n    review:\n      conversation: true\n",
		"override no_ci":                "repository_overrides:\n  https://github.com/acme/widget.git:\n    no_ci: true\n",
		"override allow_repo_commands":  "repository_overrides:\n  https://github.com/acme/widget.git:\n    allow_repo_commands: true\n",
		"override pr base_branch":       "repository_overrides:\n  https://github.com/acme/widget.git:\n    pr:\n      base_branch: develop\n",
		"override ignore_patterns":      "repository_overrides:\n  https://github.com/acme/widget.git:\n    ignore_patterns: ['**']\n",
		"override document other field": "repository_overrides:\n  https://github.com/acme/widget.git:\n    document:\n      skip: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadGlobalFromBytes([]byte(source)); err == nil {
				t.Fatalf("LoadGlobalFromBytes() accepted %q, want only review.path_instructions and document.instructions", source)
			}
		})
	}
}

func TestLoadGlobal_RejectsInvalidOperatorPathInstructions(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		source string
		want   string
	}{
		"global empty path": {
			source: "review:\n  path_instructions:\n    - path: ''\n      instructions: x\n",
			want:   "review.path_instructions[0].path must not be empty",
		},
		"global invalid glob": {
			source: "review:\n  path_instructions:\n    - path: '[unclosed'\n      instructions: x\n",
			want:   "is not a valid glob",
		},
		"override conflict markers only": {
			source: "repository_overrides:\n  https://github.com/acme/widget.git:\n    review:\n      path_instructions:\n        - path: '**'\n          instructions: '======='\n",
			want:   "repository_overrides.github.com/acme/widget: review.path_instructions[0].instructions",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadGlobalFromBytes([]byte(tc.source))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadGlobalFromBytes() error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func operatorPathInstructionsYAML(indent string, count int, prefix string) string {
	var b strings.Builder
	for i := range count {
		fmt.Fprintf(&b, "%s- path: '%s%d/**'\n%s  instructions: rule %d\n", indent, prefix, i, indent, i)
	}
	return b.String()
}

func TestLoadGlobal_OperatorPathInstructionsBudgetCountsGlobalAndOverrideTogether(t *testing.T) {
	t.Parallel()

	globalBlock := "review:\n  path_instructions:\n" + operatorPathInstructionsYAML("    ", 20, "g")
	overrideBlock := "repository_overrides:\n  https://github.com/acme/widget.git:\n    review:\n      path_instructions:\n" + operatorPathInstructionsYAML("        ", 20, "r")

	for name, source := range map[string]string{"global alone": globalBlock, "override alone": overrideBlock} {
		if _, err := LoadGlobalFromBytes([]byte(source)); err != nil {
			t.Fatalf("%s: LoadGlobalFromBytes() = %v, want 20 entries accepted", name, err)
		}
	}
	_, err := LoadGlobalFromBytes([]byte(globalBlock + overrideBlock))
	if err == nil || !strings.Contains(err.Error(), "40 entries combined (20 machine-local global, 20 machine-local per-repository)") {
		t.Fatalf("LoadGlobalFromBytes() error = %v, want the combined entry cap enforced", err)
	}
}

func TestReview_ValidatePathInstructionsBudgetCountsEverySource(t *testing.T) {
	t.Parallel()

	entries := func(n int, prefix string) []PathInstruction {
		out := make([]PathInstruction, n)
		for i := range out {
			out[i] = PathInstruction{Path: fmt.Sprintf("%s%d/**", prefix, i), Instructions: "rule"}
		}
		return out
	}

	trustedOnly := Review{PathInstructions: entries(MaxReviewPathInstructions, "t")}
	if err := trustedOnly.ValidatePathInstructionsBudget(); err != nil {
		t.Fatalf("trusted rules at the cap: %v", err)
	}

	combined := Review{PathInstructions: entries(MaxReviewPathInstructions-1, "t"), GlobalPathInstructions: entries(2, "g")}
	err := combined.ValidatePathInstructionsBudget()
	if err == nil || !strings.Contains(err.Error(), "33 entries combined (2 machine-local global, 31 trusted)") {
		t.Fatalf("ValidatePathInstructionsBudget() = %v, want the combined entry cap to name every source", err)
	}

	long := strings.Repeat("x", MaxReviewPathInstructionsBytes/2)
	oversized := Review{
		PathInstructions:           []PathInstruction{{Path: "a/**", Instructions: long}},
		RepositoryPathInstructions: []PathInstruction{{Path: "b/**", Instructions: long}},
	}
	err = oversized.ValidatePathInstructionsBudget()
	if err == nil || !strings.Contains(err.Error(), "bytes to the review prompt combined") {
		t.Fatalf("ValidatePathInstructionsBudget() = %v, want the combined byte cap enforced", err)
	}
	if got := oversized.PathInstructionsPromptBytes(); got <= MaxReviewPathInstructionsBytes {
		t.Fatalf("PathInstructionsPromptBytes() = %d, want it to exceed the cap the error reports", got)
	}
}

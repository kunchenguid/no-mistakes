package config

import (
	"slices"
	"strings"
	"testing"
)

// TestEffectiveRepoConfig_NonProductPathsTrustedOnly proves the diff-class
// classification is honored only from the trusted default-branch copy.
//
// It decides whether the live-evidence gate runs at all for a change, so a
// pushed branch that could set it could declare its own product code
// non-product and skip the live validation of exactly that code.
func TestEffectiveRepoConfig_NonProductPathsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{NonProductPaths: []string{"internal/**"}}}
	trusted := &RepoConfig{Test: TestRaw{NonProductPaths: []string{"gen/**"}}}

	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
		if !slices.Equal(effective.Test.NonProductPaths, trusted.Test.NonProductPaths) {
			t.Fatalf("NonProductPaths = %v under allow_repo_commands=%v, want the trusted list", effective.Test.NonProductPaths, allowRepoCommands)
		}
	}

	// Without a trusted copy the pushed list is discarded, which restores the
	// built-in defaults rather than the branch's own classification.
	if got := EffectiveRepoConfig(pushed, nil, false).Test.NonProductPaths; got != nil {
		t.Fatalf("without a trusted copy the pushed classification must be dropped, got %v", got)
	}
	if !slices.Equal(pushed.Test.NonProductPaths, []string{"internal/**"}) {
		t.Fatal("pushed config was mutated")
	}
}

// TestEffectiveRepoConfig_NonProductPathsEmptyListSurvivesTheTrustBoundary:
// the opt-out is the presence of the key with no entries, so it is carried by
// the slice being non-nil and empty. EffectiveRepoConfig copies the trusted
// list across the trust boundary before Merge resolves it, and a copy that
// flattened that emptiness to nil would hand Merge "the repository configured
// nothing" - restoring the built-in defaults and silently skipping the live
// validation of exactly the paths the maintainer opted back into.
func TestEffectiveRepoConfig_NonProductPathsEmptyListSurvivesTheTrustBoundary(t *testing.T) {
	trusted, err := LoadRepoFromBytes([]byte("test:\n  evidence_gate: diff-class\n  non_product_paths: []\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(&RepoConfig{}, trusted, allowRepoCommands)
		resolved := Merge(&GlobalConfig{}, effective)
		if len(resolved.Test.NonProductPaths) != 0 {
			t.Fatalf("NonProductPaths = %v under allow_repo_commands=%v, want the explicit opt-out to stay empty", resolved.Test.NonProductPaths, allowRepoCommands)
		}
	}
}

func TestMerge_NonProductPathsDefaultWhenUnset(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{})
	if !slices.Equal(got.Test.NonProductPaths, DefaultNonProductPaths) {
		t.Fatalf("NonProductPaths = %v, want the defaults", got.Test.NonProductPaths)
	}
	// The resolved slice must be a copy: a step that mutated it would rewrite
	// the defaults for every later run in the process.
	got.Test.NonProductPaths[0] = "mutated"
	if DefaultNonProductPaths[0] == "mutated" {
		t.Fatal("the resolved list aliases DefaultNonProductPaths")
	}
}

// An explicitly empty list is a deliberate opt-out - every path is product
// code - and must not silently fall back to the defaults.
func TestMerge_NonProductPathsEmptyListIsHonored(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{Test: TestRaw{NonProductPaths: []string{}}})
	if len(got.Test.NonProductPaths) != 0 {
		t.Fatalf("NonProductPaths = %v, want an empty classification", got.Test.NonProductPaths)
	}
}

func TestMerge_NonProductPathsTrimsAndDropsBlanks(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{Test: TestRaw{NonProductPaths: []string{"  gen/**  ", "   "}}})
	if !slices.Equal(got.Test.NonProductPaths, []string{"gen/**"}) {
		t.Fatalf("NonProductPaths = %v, want the trimmed single entry", got.Test.NonProductPaths)
	}
}

// The classification describes ONE repository's layout, so a global value has
// no repository to describe and must never leak into a run.
func TestMerge_GlobalNonProductPathsAreNotUsed(t *testing.T) {
	global := &GlobalConfig{Test: TestRaw{NonProductPaths: []string{"src/**"}}}
	got := Merge(global, &RepoConfig{})
	if !slices.Equal(got.Test.NonProductPaths, DefaultNonProductPaths) {
		t.Fatalf("global classification leaked into the resolved config: %v", got.Test.NonProductPaths)
	}
}

func TestLoadRepo_NonProductPaths(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("test:\n  non_product_paths:\n    - \"gen/**\"\n    - \"**/testdata/**\"\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !slices.Equal(cfg.Test.NonProductPaths, []string{"gen/**", "**/testdata/**"}) {
		t.Fatalf("NonProductPaths = %v", cfg.Test.NonProductPaths)
	}
}

// A glob that Match would reject matches nothing, which quietly turns the
// whole repository back into product code and restores the per-run evidence
// bill the gate exists to remove. Fail the config instead.
func TestLoadRepo_NonProductPathsRejectsUnusablePatterns(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{name: "malformed glob", yaml: "test:\n  non_product_paths:\n    - \"gen/[\"\n", want: "test.non_product_paths"},
		{name: "empty entry", yaml: "test:\n  non_product_paths:\n    - \"  \"\n", want: "must not be empty"},
		{name: "bare any-depth prefix", yaml: "test:\n  non_product_paths:\n    - \"**/\"\n", want: "needs a path after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected the config to fail closed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The defaults are the shipped answer to "what cannot carry a live-drivable
// change". A regression here silently changes which runs pay for the
// ~21-minute evidence turn, so the classes named in the ruling are pinned.
func TestDefaultNonProductPathsCoverEveryDeclaredClass(t *testing.T) {
	for _, pattern := range []string{
		"*.md", "docs/**", // docs and markdown
		"*_test.go", "**/testdata/**", // test files and fixtures
		".github/workflows/**", // CI workflow files
		"scripts/**",           // scripts and tooling directories
		".no-mistakes.yaml",    // the pipeline's own config
	} {
		if !slices.Contains(DefaultNonProductPaths, pattern) {
			t.Errorf("DefaultNonProductPaths is missing %q", pattern)
		}
	}
	// A lockfile change swaps the dependency versions the product ships and
	// runs, and a lockfile-only diff is the ordinary shape of a dependency
	// bump. Listing one here would hand exactly those runs an automatic
	// no-surface, which never parks, so the upgrade would ship with neither
	// live validation nor a human decision.
	for _, pattern := range []string{
		"go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "Cargo.lock",
		"poetry.lock", "uv.lock", "composer.lock", "Gemfile.lock", "Pipfile.lock",
	} {
		if slices.Contains(DefaultNonProductPaths, pattern) {
			t.Errorf("DefaultNonProductPaths must not skip lockfile %q: a dependency bump is a runtime change", pattern)
		}
	}
	// Every default must itself be a usable pattern.
	if err := validateTestRaw(TestRaw{NonProductPaths: DefaultNonProductPaths}); err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
}

// TestMerge_EvidenceGateDefaultsToAlways is the opt-in's config half: a
// repository that says nothing gets the historical behavior, where the Test
// step invokes the live-evidence agent on every run.
func TestMerge_EvidenceGateDefaultsToAlways(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{})
	if got.Test.EvidenceGate != TestEvidenceGateAlways {
		t.Fatalf("EvidenceGate = %q, want %q", got.Test.EvidenceGate, TestEvidenceGateAlways)
	}
	if DefaultTestEvidenceGate != TestEvidenceGateAlways {
		t.Fatalf("DefaultTestEvidenceGate = %q, want the historical behavior %q", DefaultTestEvidenceGate, TestEvidenceGateAlways)
	}
}

func TestMerge_EvidenceGateOptIn(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{Test: TestRaw{EvidenceGate: "  diff-class  "}})
	if got.Test.EvidenceGate != TestEvidenceGateDiffClass {
		t.Fatalf("EvidenceGate = %q, want %q", got.Test.EvidenceGate, TestEvidenceGateDiffClass)
	}
}

// An explicit "always" and a commented-out key must mean the same thing, so a
// repository can turn the gate back off without inventing a third value.
func TestMerge_EvidenceGateExplicitAlways(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{Test: TestRaw{EvidenceGate: TestEvidenceGateAlways}})
	if got.Test.EvidenceGate != TestEvidenceGateAlways {
		t.Fatalf("EvidenceGate = %q, want %q", got.Test.EvidenceGate, TestEvidenceGateAlways)
	}
}

// The gate describes ONE repository's layout and evidence economics, exactly
// like the classification it consults, so a global value must never leak in.
func TestMerge_GlobalEvidenceGateIsNotUsed(t *testing.T) {
	global := &GlobalConfig{Test: TestRaw{EvidenceGate: TestEvidenceGateDiffClass}}
	if got := Merge(global, &RepoConfig{}).Test.EvidenceGate; got != TestEvidenceGateAlways {
		t.Fatalf("global gate leaked into the resolved config: %q", got)
	}
}

// TestEffectiveRepoConfig_EvidenceGateTrustedOnly proves the switch is honored
// only from the trusted default-branch copy. A pushed branch that could set it
// could turn off the live validation of itself.
func TestEffectiveRepoConfig_EvidenceGateTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{EvidenceGate: TestEvidenceGateDiffClass}}
	trusted := &RepoConfig{Test: TestRaw{EvidenceGate: TestEvidenceGateAlways}}

	for _, allowRepoCommands := range []bool{false, true} {
		if got := EffectiveRepoConfig(pushed, trusted, allowRepoCommands).Test.EvidenceGate; got != TestEvidenceGateAlways {
			t.Fatalf("EvidenceGate = %q under allow_repo_commands=%v, want the trusted %q", got, allowRepoCommands, TestEvidenceGateAlways)
		}
	}
	if got := EffectiveRepoConfig(pushed, nil, true).Test.EvidenceGate; got != "" {
		t.Fatalf("without a trusted copy the pushed gate must be dropped, got %q", got)
	}
}

// A typo must fail the config rather than silently resolving to a gate the
// maintainer did not choose, in either direction. Mirrors rebase.strategy.
func TestLoadRepo_EvidenceGateRejectsUnknownValue(t *testing.T) {
	for _, value := range []string{"diffclass", "diff_class", "never", "off"} {
		_, err := LoadRepoFromBytes([]byte("test:\n  evidence_gate: " + value + "\n"))
		if err == nil {
			t.Fatalf("expected %q to fail the config closed", value)
		}
		if !strings.Contains(err.Error(), "test.evidence_gate") {
			t.Fatalf("error for %q = %v, want it to name the field", value, err)
		}
	}
	for _, value := range []string{"always", "diff-class"} {
		cfg, err := LoadRepoFromBytes([]byte("test:\n  evidence_gate: " + value + "\n"))
		if err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
		if cfg.Test.EvidenceGate != value {
			t.Fatalf("EvidenceGate = %q, want %q", cfg.Test.EvidenceGate, value)
		}
	}
}

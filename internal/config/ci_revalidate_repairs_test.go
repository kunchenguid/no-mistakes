package config

import (
	"strings"
	"testing"
)

// resolveRevalidateRepairs runs one global YAML document and one repository
// YAML document through the real loaders, the trusted/pushed effective-config
// rule, and Merge, and reports the policy the pipeline would actually see.
// Going through the loaders (rather than constructing structs) is what makes
// these assertions cover YAML key naming, strict-field checking, and the
// pointer semantics that let an explicit false override an inherited true.
func resolveRevalidateRepairs(t *testing.T, globalYAML, trustedRepoYAML, pushedRepoYAML string) bool {
	t.Helper()
	global, err := LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes(%q): %v", globalYAML, err)
	}
	trusted, err := LoadRepoFromBytes([]byte(trustedRepoYAML))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes(trusted %q): %v", trustedRepoYAML, err)
	}
	pushed, err := LoadRepoFromBytes([]byte(pushedRepoYAML))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes(pushed %q): %v", pushedRepoYAML, err)
	}
	return Merge(global, EffectiveRepoConfig(pushed, trusted, false)).CI.RevalidateRepairs
}

func TestCIRevalidateRepairs_GlobalAndProjectPrecedence(t *testing.T) {
	t.Parallel()
	const on = "ci:\n  revalidate_repairs: true\n"
	const off = "ci:\n  revalidate_repairs: false\n"
	const unset = "{}\n"

	for _, tc := range []struct {
		name    string
		global  string
		trusted string
		want    bool
		why     string
	}{
		{
			name: "absent_everywhere_defaults_to_publishing_the_repair", global: unset, trusted: unset, want: false,
			why: "the expensive full revalidation must never be paid for by a config that never asked for it",
		},
		{
			name: "global_true_selects_revalidation", global: on, trusted: unset, want: true,
			why: "an operator can turn it on machine-wide",
		},
		{
			name: "project_true_selects_revalidation", global: unset, trusted: on, want: true,
			why: "a repository can require it without the operator configuring anything",
		},
		{
			name: "project_false_overrides_global_true", global: on, trusted: off, want: false,
			why: "an explicit project false is a real value, not an absent one",
		},
		{
			name: "project_true_overrides_global_false", global: off, trusted: on, want: true,
			why: "precedence runs in the normal direction in both directions",
		},
		{
			name: "global_true_survives_a_project_that_sets_only_the_rerun_budget", global: on,
			trusted: "ci:\n  rerun_transient: 2\n", want: true,
			why: "setting one key in the ci block must not silently clear the other",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveRevalidateRepairs(t, tc.global, tc.trusted, unset); got != tc.want {
				t.Fatalf("CI.RevalidateRepairs = %v, want %v: %s", got, tc.want, tc.why)
			}
		})
	}
}

// A pushed branch must not be able to turn its maintainer's revalidation
// requirement off for its own repairs, so the whole ci block is read from the
// trusted default-branch copy. Both directions are pinned: a pushed false
// cannot weaken a trusted true, and a pushed true cannot impose cost the
// maintainer did not ask for either.
func TestCIRevalidateRepairs_TrustedOnly(t *testing.T) {
	t.Parallel()
	const on = "ci:\n  revalidate_repairs: true\n"
	const off = "ci:\n  revalidate_repairs: false\n"
	const unset = "{}\n"

	if got := resolveRevalidateRepairs(t, unset, on, off); !got {
		t.Error("a pushed branch disabled the trusted revalidation requirement")
	}
	if got := resolveRevalidateRepairs(t, unset, off, on); got {
		t.Error("a pushed branch enabled revalidation the trusted config declined")
	}
	if got := resolveRevalidateRepairs(t, unset, unset, on); got {
		t.Error("a pushed branch enabled revalidation with no trusted config at all")
	}
}

// The opt-in is a security-relevant boundary, so it stays trusted-only even
// when a repository has taken the allow_repo_commands opt-in that hands the
// pushed branch control of what executes.
func TestCIRevalidateRepairs_TrustedOnlyEvenWithRepoCommandsOptIn(t *testing.T) {
	t.Parallel()
	trusted, err := LoadRepoFromBytes([]byte("allow_repo_commands: true\nci:\n  revalidate_repairs: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := LoadRepoFromBytes([]byte("ci:\n  revalidate_repairs: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !Merge(global, EffectiveRepoConfig(pushed, trusted, true)).CI.RevalidateRepairs {
		t.Error("allow_repo_commands let a pushed branch disable revalidation")
	}
}

func TestCIRevalidateRepairs_RejectsNonBooleanGlobalValue(t *testing.T) {
	t.Parallel()
	_, err := LoadGlobalFromBytes([]byte("ci:\n  revalidate_repairs: sometimes\n"))
	if err == nil {
		t.Fatal("expected a non-boolean revalidate_repairs to be rejected")
	}
	if !strings.Contains(err.Error(), "parse global config") {
		t.Fatalf("error = %v, want a global config parse failure", err)
	}
}

func TestCIRevalidateRepairs_RejectsNonBooleanRepoValue(t *testing.T) {
	t.Parallel()
	if _, err := LoadRepoFromBytes([]byte("ci:\n  revalidate_repairs: sometimes\n")); err == nil {
		t.Fatal("expected a non-boolean revalidate_repairs to be rejected")
	}
}

// The shipped default config must document the key it ships, and must show the
// default rather than an aspirational value.
func TestCIRevalidateRepairs_ShippedDefaultConfigParsesAndKeepsTheDefault(t *testing.T) {
	t.Parallel()
	cfg, err := LoadGlobalFromBytes([]byte(defaultConfigYAML))
	if err != nil {
		t.Fatalf("shipped example global config does not parse: %v", err)
	}
	repo, err := LoadRepoFromBytes([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(cfg, repo).CI.RevalidateRepairs {
		t.Error("the shipped default config enables revalidation; the documented default is false")
	}
}

package config

import (
	"strings"
	"testing"
)

// resolveRebaseStrategy runs one global YAML document and one repository pair
// (trusted default-branch copy + pushed-branch copy) through the same path the
// daemon uses, and returns the strategy the rebase step would see.
func resolveRebaseStrategy(t *testing.T, globalYAML, trustedRepoYAML, pushedRepoYAML string) string {
	t.Helper()
	global, err := LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatalf("load global config: %v", err)
	}
	trusted, err := LoadRepoFromBytes([]byte(trustedRepoYAML))
	if err != nil {
		t.Fatalf("load trusted repo config: %v", err)
	}
	pushed, err := LoadRepoFromBytes([]byte(pushedRepoYAML))
	if err != nil {
		t.Fatalf("load pushed repo config: %v", err)
	}
	return Merge(global, EffectiveRepoConfig(pushed, trusted, false)).Rebase.Strategy
}

// unset is an empty YAML document: the loaders reject a zero-byte one.
const unsetRebaseYAML = "{}\n"

func TestRebaseStrategy_DefaultsToRebase(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	if got := resolveRebaseStrategy(t, unset, unset, unset); got != RebaseStrategyRebase {
		t.Fatalf("Rebase.Strategy = %q, want %q for an unset config", got, RebaseStrategyRebase)
	}
}

func TestRebaseStrategy_GlobalAndProjectPrecedence(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	const merge = "rebase:\n  strategy: merge\n"
	const rebase = "rebase:\n  strategy: rebase\n"

	for _, tc := range []struct {
		name            string
		global, trusted string
		want            string
		why             string
	}{
		{"global only", merge, unset, RebaseStrategyMerge, "the operator's machine-wide default applies"},
		{"repo only", unset, merge, RebaseStrategyMerge, "a repository can select the merge shape on its own"},
		{"repo overrides global", merge, rebase, RebaseStrategyRebase, "the maintainer of the repository has the last word"},
		{"repo opts in over global rebase", rebase, merge, RebaseStrategyMerge, "an explicit repo value wins in both directions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRebaseStrategy(t, tc.global, tc.trusted, unset); got != tc.want {
				t.Fatalf("Rebase.Strategy = %q, want %q: %s", got, tc.want, tc.why)
			}
		})
	}
}

// rebase.strategy decides whether integrating a moved base leaves an auditable
// merge commit behind, so a pushed branch must not be able to change it in
// either direction.
func TestRebaseStrategy_TrustedOnly(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	const merge = "rebase:\n  strategy: merge\n"
	const rebase = "rebase:\n  strategy: rebase\n"

	if got := resolveRebaseStrategy(t, unset, merge, rebase); got != RebaseStrategyMerge {
		t.Fatalf("Rebase.Strategy = %q, want %q: a pushed branch must not opt out of the trusted merge shape", got, RebaseStrategyMerge)
	}
	if got := resolveRebaseStrategy(t, unset, unset, merge); got != RebaseStrategyRebase {
		t.Fatalf("Rebase.Strategy = %q, want %q: a pushed branch must not select the shape on its own", got, RebaseStrategyRebase)
	}
}

func TestRebaseStrategy_TrustedOnlyEvenWithRepoCommandsOptIn(t *testing.T) {
	t.Parallel()
	global, err := LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("allow_repo_commands: true\nrebase:\n  strategy: merge\n"))
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := LoadRepoFromBytes([]byte("rebase:\n  strategy: rebase\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(global, EffectiveRepoConfig(pushed, trusted, true)).Rebase.Strategy; got != RebaseStrategyMerge {
		t.Fatalf("Rebase.Strategy = %q, want %q: the commands opt-in covers what a branch runs, not the shape of its integration", got, RebaseStrategyMerge)
	}
}

func TestRebaseStrategy_RejectsUnknownValue(t *testing.T) {
	t.Parallel()
	if _, err := LoadRepoFromBytes([]byte("rebase:\n  strategy: merges\n")); err == nil {
		t.Fatal("expected an unknown repo rebase.strategy to fail the config closed")
	} else if !strings.Contains(err.Error(), "rebase.strategy") {
		t.Fatalf("error does not name the setting: %v", err)
	}
	if _, err := LoadGlobalFromBytes([]byte("rebase:\n  strategy: squash\n")); err == nil {
		t.Fatal("expected an unknown global rebase.strategy to fail the config closed")
	}
}

// An empty value means "not set" rather than a third strategy, so commenting
// the value out cannot silently invent behavior.
func TestRebaseStrategy_EmptyValueKeepsTheDefault(t *testing.T) {
	t.Parallel()
	if got := resolveRebaseStrategy(t, "rebase:\n  strategy: \"\"\n", unsetRebaseYAML, unsetRebaseYAML); got != RebaseStrategyRebase {
		t.Fatalf("Rebase.Strategy = %q, want %q for an empty value", got, RebaseStrategyRebase)
	}
}

package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const ceilingTestRemote = "https://github.com/acme/widget.git"

func mergeCeilingCase(t *testing.T, globalYAML, repoYAML, remote string) *Config {
	t.Helper()
	global, err := LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() error = %v", err)
	}
	repo, err := LoadRepoFromBytes([]byte(repoYAML))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes() error = %v", err)
	}
	return MergeForRemote(global, repo, remote)
}

func TestAutoFixCeiling_UnsetKeepsTodaysResolution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, global, repo string
	}{
		{name: "defaults", global: "{}\n"},
		{name: "global auto_fix", global: "auto_fix:\n  ci: 5\n  lint: 1\n"},
		{name: "repo auto_fix", global: "auto_fix:\n  ci: 1\n", repo: "auto_fix:\n  ci: 7\n  review: 2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tc.global))
			if err != nil {
				t.Fatal(err)
			}
			repo, err := LoadRepoFromBytes([]byte(tc.repo))
			if err != nil {
				t.Fatal(err)
			}
			want := autoFixDefaults()
			applyAutoFixOverrides(&want, &global.AutoFix)
			applyAutoFixOverrides(&want, &repo.AutoFix)
			got := MergeForRemote(global, repo, ceilingTestRemote)
			for _, step := range []types.StepName{types.StepLint, types.StepTest, types.StepReview, types.StepDocument, types.StepCI, types.StepRebase} {
				w := (&Config{AutoFix: want}).AutoFixLimit(step)
				if g := got.AutoFixLimit(step); g != w {
					t.Errorf("AutoFixLimit(%s) = %d, want unchanged %d", step, g, w)
				}
			}
			if len(got.AutoFixCapped) != 0 {
				t.Errorf("AutoFixCapped = %v, want empty without a ceiling", got.AutoFixCapped)
			}
		})
	}
}

func TestAutoFixCeiling_RepoConfigCannotRaiseIt(t *testing.T) {
	t.Parallel()

	cfg := mergeCeilingCase(t, "auto_fix_ceiling:\n  ci: 0\n", "auto_fix:\n  ci: 3\n  lint: 2\n", ceilingTestRemote)
	if got := cfg.AutoFixLimit(types.StepCI); got != 0 {
		t.Fatalf("AutoFixLimit(ci) = %d, want ceiling 0 over repo 3", got)
	}
	if got := cfg.AutoFixLimit(types.StepLint); got != 2 {
		t.Fatalf("AutoFixLimit(lint) = %d, want repo 2 (no lint ceiling)", got)
	}
	if got, ok := cfg.AutoFixCapped[types.StepCI]; !ok || got != 3 {
		t.Fatalf("AutoFixCapped[ci] = %d, %v, want requested 3 recorded", got, ok)
	}
	if _, ok := cfg.AutoFixCapped[types.StepLint]; ok {
		t.Fatalf("AutoFixCapped records lint, which no ceiling lowered")
	}
}

func TestAutoFixCeiling_RepoConfigCanLowerBelowIt(t *testing.T) {
	t.Parallel()

	cfg := mergeCeilingCase(t, "auto_fix_ceiling:\n  ci: 2\n", "auto_fix:\n  ci: 1\n", ceilingTestRemote)
	if got := cfg.AutoFixLimit(types.StepCI); got != 1 {
		t.Fatalf("AutoFixLimit(ci) = %d, want repo 1 under ceiling 2", got)
	}
	if len(cfg.AutoFixCapped) != 0 {
		t.Fatalf("AutoFixCapped = %v, want empty when nothing was lowered", cfg.AutoFixCapped)
	}
}

func TestAutoFixCeiling_CapsTheGlobalAutoFixToo(t *testing.T) {
	t.Parallel()

	cfg := mergeCeilingCase(t, "auto_fix:\n  test: 5\nauto_fix_ceiling:\n  test: 1\n", "", ceilingTestRemote)
	if got := cfg.AutoFixLimit(types.StepTest); got != 1 {
		t.Fatalf("AutoFixLimit(test) = %d, want ceiling 1 over global 5", got)
	}
}

func TestAutoFixCeiling_RepositoryOverrideAppliesToItsRemoteOnly(t *testing.T) {
	t.Parallel()

	global := `auto_fix_ceiling:
  ci: 0
repository_overrides:
  ` + ceilingTestRemote + `:
    auto_fix_ceiling:
      ci: 2
      rebase: 1
`
	repo := "auto_fix:\n  ci: 5\n  rebase: 4\n"

	matched := mergeCeilingCase(t, global, repo, "git@github.com:acme/widget.git")
	if got := matched.AutoFixLimit(types.StepCI); got != 2 {
		t.Fatalf("matched AutoFixLimit(ci) = %d, want override ceiling 2 replacing global 0", got)
	}
	if got := matched.AutoFixLimit(types.StepRebase); got != 1 {
		t.Fatalf("matched AutoFixLimit(rebase) = %d, want override ceiling 1", got)
	}

	other := mergeCeilingCase(t, global, repo, "https://github.com/acme/other.git")
	if got := other.AutoFixLimit(types.StepCI); got != 0 {
		t.Fatalf("other AutoFixLimit(ci) = %d, want global ceiling 0", got)
	}
	if got := other.AutoFixLimit(types.StepRebase); got != 4 {
		t.Fatalf("other AutoFixLimit(rebase) = %d, want repo 4 (override must not leak)", got)
	}
}

func TestAutoFixCeiling_RepositoryOverrideWithoutGlobal(t *testing.T) {
	t.Parallel()

	global := "repository_overrides:\n  " + ceilingTestRemote + ":\n    auto_fix_ceiling:\n      review: 0\n"
	repo := "auto_fix:\n  review: 3\n"
	if got := mergeCeilingCase(t, global, repo, ceilingTestRemote).AutoFixLimit(types.StepReview); got != 0 {
		t.Fatalf("matched AutoFixLimit(review) = %d, want override ceiling 0", got)
	}
	if got := mergeCeilingCase(t, global, repo, "https://github.com/acme/other.git").AutoFixLimit(types.StepReview); got != 3 {
		t.Fatalf("other AutoFixLimit(review) = %d, want repo 3", got)
	}
}

func TestAutoFixCeiling_RejectsNegativeValues(t *testing.T) {
	t.Parallel()

	for name, yaml := range map[string]string{
		"global":   "auto_fix_ceiling:\n  ci: -1\n",
		"override": "repository_overrides:\n  " + ceilingTestRemote + ":\n    auto_fix_ceiling:\n      lint: -2\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(yaml))
			if err == nil || !strings.Contains(err.Error(), "auto_fix_ceiling") || !strings.Contains(err.Error(), "non-negative") {
				t.Fatalf("LoadGlobalFromBytes() error = %v, want non-negative auto_fix_ceiling error", err)
			}
		})
	}
}

func TestAutoFixCeiling_RejectsUnknownStep(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"babysit", "gates"} {
		if _, err := LoadGlobalFromBytes([]byte("auto_fix_ceiling:\n  " + key + ": 1\n")); err == nil {
			t.Fatalf("auto_fix_ceiling.%s accepted, want strict decode to refuse it", key)
		}
	}
}

func TestAutoFixCeiling_RepoConfigCannotSetIt(t *testing.T) {
	t.Parallel()

	_, err := LoadRepoFromBytes([]byte("auto_fix_ceiling:\n  ci: 9\n"))
	if err == nil || !strings.Contains(err.Error(), "auto_fix_ceiling is global-only") {
		t.Fatalf("LoadRepoFromBytes() error = %v, want auto_fix_ceiling refused as global-only", err)
	}
}

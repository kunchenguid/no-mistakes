package config

import (
	"slices"
	"testing"
)

func TestEffectiveRepoConfig_TrustedReviewIgnores(t *testing.T) {
	t.Parallel()
	for _, allow := range []bool{false, true} {
		pushed := &RepoConfig{
			IgnorePatterns:        []string{"*"},
			TrustedIgnorePatterns: []string{"forged/**"},
		}
		trusted := &RepoConfig{IgnorePatterns: []string{"vendor/**"}}
		effective := EffectiveRepoConfig(pushed, trusted, allow)
		cfg := Merge(DefaultGlobalConfig(), effective)
		if !slices.Equal(cfg.IgnorePatterns, []string{"*"}) {
			t.Fatalf("pushed ignores changed: %v", cfg.IgnorePatterns)
		}
		if !slices.Equal(cfg.TrustedIgnorePatterns, []string{"vendor/**"}) {
			t.Fatalf("allow_repo_commands=%v: trusted ignores=%v", allow, cfg.TrustedIgnorePatterns)
		}
		trusted.IgnorePatterns[0] = "changed/**"
		if cfg.TrustedIgnorePatterns[0] != "vendor/**" {
			t.Fatal("resolved trusted ignores alias their source")
		}
		withoutTrusted := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, nil, allow))
		if len(withoutTrusted.TrustedIgnorePatterns) != 0 {
			t.Fatalf("pushed config supplied trusted ignores: %v", withoutTrusted.TrustedIgnorePatterns)
		}
		if pushed.TrustedIgnorePatterns[0] != "forged/**" {
			t.Fatal("effective config mutated the pushed copy")
		}
	}
}

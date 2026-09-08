package config

import "testing"

func TestPRPublishIntentTrustedEvenWithCommandsOptIn(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	pushed := &RepoConfig{PR: PRRaw{BaseBranch: "feature-base", PublishIntent: &yes}}
	trusted := &RepoConfig{PR: PRRaw{BaseBranch: "trusted-base", PublishIntent: &no}}
	for _, allow := range []bool{false, true} {
		got := EffectiveRepoConfig(pushed, trusted, allow)
		cfg := Merge(DefaultGlobalConfig(), got)
		if cfg.PR.PublishesIntent() {
			t.Fatalf("allow=%v: pushed publication policy won: %+v", allow, cfg.PR)
		}
		wantBase := "trusted-base"
		if allow {
			wantBase = "feature-base"
		}
		if cfg.PR.BaseBranch != wantBase {
			t.Fatalf("base-branch opt-in semantics changed: %+v", cfg.PR)
		}
		for _, absent := range []*RepoConfig{nil, {}} {
			got = EffectiveRepoConfig(pushed, absent, allow)
			if got.PR.PublishIntent != nil {
				t.Fatalf("allow=%v: absent trusted policy inherited pushed value: %+v", allow, got.PR)
			}
		}
		got = EffectiveRepoConfig(nil, trusted, allow)
		if got.PR.PublishIntent == nil || *got.PR.PublishIntent {
			t.Fatalf("allow=%v: trusted policy lost when pushed copy absent", allow)
		}
	}
	if !*pushed.PR.PublishIntent {
		t.Fatal("trust merge mutated caller input")
	}
}

func TestPRPublishIntentDefaultsAndParsing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		yaml string
		want bool
	}{
		{"{}", true}, {"pr: {}", true}, {"pr: {publish_intent: null}", true},
		{"pr: {publish_intent: true}", true}, {"pr: {publish_intent: false}", false},
	} {
		repo, err := LoadRepoFromBytes([]byte(tc.yaml))
		if err != nil {
			t.Fatal(err)
		}
		if got := Merge(DefaultGlobalConfig(), repo).PR.PublishesIntent(); got != tc.want {
			t.Errorf("%s: publish=%v, want %v", tc.yaml, got, tc.want)
		}
	}
	if !(PR{}).PublishesIntent() {
		t.Fatal("zero-value config must preserve default publication")
	}
	if _, err := LoadRepoFromBytes([]byte("pr: {publish_intent: [false]}")); err == nil {
		t.Fatal("invalid publication boolean accepted")
	}
}

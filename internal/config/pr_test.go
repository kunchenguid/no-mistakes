package config

import (
	"fmt"
	"testing"
)

func TestPRPublicationPolicyTrustedEvenWithCommandsOptIn(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	pushed := &RepoConfig{PR: PRRaw{BaseBranch: "feature-base", Template: "evil.md", PublishIntent: &yes}}
	trusted := &RepoConfig{PR: PRRaw{BaseBranch: "trusted-base", Template: ".github/pull_request_template.md", PublishIntent: &no}}
	for _, allow := range []bool{false, true} {
		got := EffectiveRepoConfig(pushed, trusted, allow)
		cfg := Merge(DefaultGlobalConfig(), got)
		if cfg.PR.Template != trusted.PR.Template || cfg.PR.PublishesIntent() {
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
			if got.PR.Template != "" || got.PR.PublishIntent != nil {
				t.Fatalf("allow=%v: absent trusted policy inherited pushed values: %+v", allow, got.PR)
			}
		}
		got = EffectiveRepoConfig(nil, trusted, allow)
		if got.PR.Template != trusted.PR.Template || got.PR.PublishIntent == nil || *got.PR.PublishIntent {
			t.Fatalf("allow=%v: trusted policy lost when pushed copy absent", allow)
		}
	}
	if pushed.PR.Template != "evil.md" || !*pushed.PR.PublishIntent {
		t.Fatal("trust merge mutated caller input")
	}
}

func TestPRTemplateLiteralPaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", ".github/pull_request_template.md", "docs/PR template.md", "templates/[team].md"} {
		if err := ValidatePRTemplatePath(name); err != nil {
			t.Errorf("valid %q: %v", name, err)
		}
		repo, err := LoadRepoFromBytes([]byte(fmt.Sprintf("pr:\n  template: %q\n", name)))
		if err != nil || Merge(DefaultGlobalConfig(), repo).PR.Template != name {
			t.Errorf("path parse/merge %q: %v", name, err)
		}
	}
	for _, name := range []string{".", "..", "../outside", "a/../../outside", "/etc/passwd", `C:\Users\who\x`, `a\b`, "https://example.test/a", "main:x", "./a", "a//b", "a/", ".git/config", "a/.git/x", " x", "x\n", "a\x00b", "-template"} {
		if err := ValidatePRTemplatePath(name); err == nil {
			t.Errorf("unsafe %q accepted", name)
		}
	}
}

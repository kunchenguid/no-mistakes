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

func TestPRRenderTitle_DefaultLeavesTitleUnchanged(t *testing.T) {
	t.Parallel()

	got, err := (PR{}).RenderTitle("PROJ-123", "fix: repair widget")
	if err != nil {
		t.Fatal(err)
	}
	if want := "fix: repair widget"; got != want {
		t.Fatalf("RenderTitle() = %q, want %q", got, want)
	}
}

func TestPRRenderTitle_CustomFormat(t *testing.T) {
	t.Parallel()

	pr := PR{TitleFormat: "{{.Branch}}: {{.Title}}"}
	got, err := pr.RenderTitle("PROJ-123", "add widget")
	if err != nil {
		t.Fatal(err)
	}
	if want := "PROJ-123: add widget"; got != want {
		t.Fatalf("RenderTitle() = %q, want %q", got, want)
	}
}

func TestPRRenderTitle_NoIdentifierFailsClosed(t *testing.T) {
	t.Parallel()

	pr := PR{TitleFormat: "{{.Branch}}: {{.Title}}"}
	if got, err := pr.RenderTitle("", "add widget"); err == nil {
		t.Fatalf("RenderTitle() = %q, want no-identifier error", got)
	}
}

func TestLoadRepo_PRTitleFormat(t *testing.T) {
	t.Parallel()

	cfg, err := LoadRepoFromBytes([]byte("pr:\n  title_format: '{{.Branch}}: {{.Title}}'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PR.TitleFormat == nil || *cfg.PR.TitleFormat != "{{.Branch}}: {{.Title}}" {
		t.Fatalf("pr.title_format = %v, want configured format", cfg.PR.TitleFormat)
	}
}

func TestLoadRepo_RejectsInvalidPRTitleFormat(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":               "pr:\n  title_format: ''\n",
		"unknown placeholder": "pr:\n  title_format: '{{.Summary}}'\n",
		"function":            "pr:\n  title_format: '{{printf \"%s\" .Title}}'\n",
		"malformed":           "pr:\n  title_format: '{{'\n",
		"control":             "pr:\n  title_format: \"PROJ-123:\\u0007 {{.Title}}\"\n",
	}
	for name, data := range tests {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadRepoFromBytes([]byte(data)); err == nil {
				t.Fatal("LoadRepoFromBytes() accepted invalid pr.title_format")
			}
		})
	}
}

func TestEffectiveRepoConfig_TitleFormatIsPushedReadable(t *testing.T) {
	t.Parallel()

	pushedFormat := "{{.Branch}}: {{.Title}}"
	trustedFormat := "trusted {{.Title}}"
	pushed := &RepoConfig{PR: PRRaw{TitleFormat: &pushedFormat}}
	trusted := &RepoConfig{PR: PRRaw{TitleFormat: &trustedFormat, BaseBranch: "develop"}}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if got.PR.TitleFormat == nil || *got.PR.TitleFormat != pushedFormat {
		t.Fatalf("effective title_format = %v, want pushed format", got.PR.TitleFormat)
	}
	if got.PR.BaseBranch != "develop" {
		t.Fatalf("effective base_branch = %q, want trusted branch", got.PR.BaseBranch)
	}
}

func TestMerge_DefaultPRTitleFormatIsEmpty(t *testing.T) {
	t.Parallel()

	cfg := Merge(DefaultGlobalConfig(), &RepoConfig{})
	if cfg.PR.TitleFormat != "" {
		t.Fatalf("PR.TitleFormat = %q, want empty default", cfg.PR.TitleFormat)
	}
}

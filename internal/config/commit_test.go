package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommitRenderFixMessage_Default(t *testing.T) {
	t.Parallel()

	got, err := (Commit{}).RenderFixMessage(types.StepReview, "address review findings")
	if err != nil {
		t.Fatal(err)
	}
	want := "no-mistakes(review): address review findings"
	if got != want {
		t.Fatalf("RenderFixMessage() = %q, want %q", got, want)
	}
}

func TestCommitRenderFixMessage_CustomTemplate(t *testing.T) {
	t.Parallel()

	commit := Commit{FixMessage: "chore(no-mistakes-{{.Step}}): {{.Summary}}"}
	got, err := commit.RenderFixMessage(types.StepDocument, "update configuration docs")
	if err != nil {
		t.Fatal(err)
	}
	want := "chore(no-mistakes-document): update configuration docs"
	if got != want {
		t.Fatalf("RenderFixMessage() = %q, want %q", got, want)
	}
}

func TestCommitRenderFixMessage_RejectsOversizedTemplateSource(t *testing.T) {
	t.Parallel()

	commit := Commit{FixMessage: strings.Repeat("x", maxFixMessageTemplateBytes+1)}
	if _, err := commit.RenderFixMessage(types.StepReview, "apply fixes"); err == nil {
		t.Fatal("RenderFixMessage() accepted an oversized template source")
	}
}

func TestCommitRenderFixMessage_RejectsTooManyPlaceholders(t *testing.T) {
	t.Parallel()

	commit := Commit{FixMessage: strings.Repeat("{{.Summary}}", maxFixMessagePlaceholders+1)}
	if _, err := commit.RenderFixMessage(types.StepReview, "apply fixes"); err == nil {
		t.Fatal("RenderFixMessage() accepted too many placeholders")
	}
}

func TestCommitRenderFixMessage_RejectsOversizedSummary(t *testing.T) {
	t.Parallel()

	summary := strings.Repeat("x", MaxFixMessageSummaryBytes+1)
	if _, err := (Commit{}).RenderFixMessage(types.StepReview, summary); err == nil {
		t.Fatal("RenderFixMessage() accepted an oversized summary")
	}
}

func TestCommitRenderFixMessage_RejectsOversizedRenderedMessage(t *testing.T) {
	t.Parallel()

	summary := strings.Repeat("x", maxFixMessageSubjectBytes/2+1)
	commit := Commit{FixMessage: "{{.Summary}}{{.Summary}}"}
	if _, err := commit.RenderFixMessage(types.StepReview, summary); err == nil {
		t.Fatal("RenderFixMessage() accepted an oversized rendered message")
	}
}

func TestCommitRenderFixMessage_AcceptsSizeBoundaries(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		template string
		summary  string
	}{
		"template source": {template: strings.Repeat("x", maxFixMessageTemplateBytes)},
		"placeholders":    {template: strings.Repeat("{{.Summary}}", maxFixMessagePlaceholders), summary: "x"},
		"summary":         {template: "{{.Summary}}", summary: strings.Repeat("x", MaxFixMessageSummaryBytes)},
		"rendered message": {
			template: "{{.Summary}}{{.Summary}}",
			summary:  strings.Repeat("x", maxFixMessageSubjectBytes/2),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (Commit{FixMessage: tt.template}).RenderFixMessage(types.StepReview, tt.summary); err != nil {
				t.Fatalf("RenderFixMessage() rejected an exact boundary value: %v", err)
			}
		})
	}
}

func TestCommitRenderFixMessage_NormalizesMultilineSummary(t *testing.T) {
	t.Parallel()

	got, err := (Commit{}).RenderFixMessage(types.StepDocument, "update configuration\n\tdocs")
	if err != nil {
		t.Fatal(err)
	}
	want := "no-mistakes(document): update configuration docs"
	if got != want {
		t.Fatalf("RenderFixMessage() = %q, want %q", got, want)
	}
}

func TestCommitRenderFixMessage_RejectsUnsafeCharacters(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"invalid UTF-8":                          "chore:\xff {{.Summary}}",
		"bell control":                           "chore:\a {{.Summary}}",
		"escape control":                         "chore:\x1b {{.Summary}}",
		"unicode line separator":                 "chore:\u2028{{.Summary}}",
		"unicode paragraph separator":            "chore:\u2029{{.Summary}}",
		"right-to-left override":                 "chore:\u202e{{.Summary}}",
		"bidi override in comment":               "{{/*\u202e*/}}chore: {{.Summary}}",
		"left-to-right isolate":                  "chore:\u2066{{.Summary}}",
		"deprecated bidi formatting control":     "chore:\u206a{{.Summary}}",
		"deprecated control in template comment": "{{/*\u206a*/}}chore: {{.Summary}}",
		"zero-width space":                       "chore:\u200b{{.Summary}}",
		"word joiner":                            "chore:\u2060{{.Summary}}",
		"soft hyphen":                            "chore:\u00ad{{.Summary}}",
		"Unicode tag character":                  "chore:\U000e0061{{.Summary}}",
	}
	for name, source := range tests {
		name, source := name, source
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (Commit{FixMessage: source}).RenderFixMessage(types.StepReview, "apply fixes"); err == nil {
				t.Fatal("RenderFixMessage() accepted an unsafe character")
			}
		})
	}
}

func TestCommitRenderFixMessage_RejectsUnsafeSummaryCharacters(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"invalid UTF-8":                      "apply \xff fixes",
		"right-to-left override":             "apply \u202e fixes",
		"deprecated bidi formatting control": "apply \u206a fixes",
		"zero-width space":                   "apply \u200b fixes",
		"Unicode tag character":              "apply \U000e0061 fixes",
	}
	for name, summary := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (Commit{}).RenderFixMessage(types.StepReview, summary); err == nil {
				t.Fatal("RenderFixMessage() accepted an unsafe summary character")
			}
		})
	}
}

func TestCommitRenderFixMessage_AllowsLegitimateJoinControls(t *testing.T) {
	t.Parallel()

	for name, summary := range map[string]string{
		"emoji zero-width joiner":       "support 👩‍💻 workflows",
		"Persian zero-width non-joiner": "fix می‌رود rendering",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (Commit{}).RenderFixMessage(types.StepReview, summary); err != nil {
				t.Fatalf("RenderFixMessage() rejected a legitimate join control: %v", err)
			}
		})
	}
}

func TestLoadGlobal_CommitFixMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const source = "chore(no-mistakes-{{.Step}}): {{.Summary}}"
	data := []byte("commit:\n  fix_message: '" + source + "'\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commit.FixMessage == nil || *cfg.Commit.FixMessage != source {
		t.Fatalf("commit.fix_message = %v, want %q", cfg.Commit.FixMessage, source)
	}
}

func TestLoadGlobal_RejectsInvalidCommitFixMessage(t *testing.T) {
	tests := map[string]string{
		"unknown variable":      "commit:\n  fix_message: '{{.Unknown}}'\n",
		"template function":     "commit:\n  fix_message: '{{printf \"%s\" .Summary}}'\n",
		"conditional":           "commit:\n  fix_message: '{{if .Summary}}{{.Summary}}{{end}}'\n",
		"named template":        "commit:\n  fix_message: '{{define \"loop\"}}{{template \"loop\"}}{{end}}{{template \"loop\"}}'\n",
		"malformed syntax":      "commit:\n  fix_message: '{{'\n",
		"empty template":        "commit:\n  fix_message: ''\n",
		"oversized template":    "commit:\n  fix_message: '" + strings.Repeat("x", maxFixMessageTemplateBytes+1) + "'\n",
		"too many placeholders": "commit:\n  fix_message: '" + strings.Repeat("{{.Summary}}", maxFixMessagePlaceholders+1) + "'\n",
		"multiline output":      "commit:\n  fix_message: |-\n    first line\n    second line\n",
		"bell control":          "commit:\n  fix_message: \"chore:\\u0007 {{.Summary}}\"\n",
		"escape control":        "commit:\n  fix_message: \"chore:\\u001b {{.Summary}}\"\n",
		"line separator":        "commit:\n  fix_message: \"chore:\\u2028{{.Summary}}\"\n",
		"paragraph separator":   "commit:\n  fix_message: \"chore:\\u2029{{.Summary}}\"\n",
		"bidi override":         "commit:\n  fix_message: \"chore:\\u202e{{.Summary}}\"\n",
		"zero-width space":      "commit:\n  fix_message: \"chore:\\u200b{{.Summary}}\"\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := LoadGlobal(path); err == nil {
				t.Fatal("LoadGlobal() accepted an invalid commit.fix_message")
			}
		})
	}
}

func TestLoadRepo_CommitFixMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	const source = "{{.Summary}}"
	data := []byte("commit:\n  fix_message: '" + source + "'\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commit.FixMessage == nil || *cfg.Commit.FixMessage != source {
		t.Fatalf("commit.fix_message = %v, want %q", cfg.Commit.FixMessage, source)
	}
}

func TestLoadRepo_RejectsInvalidCommitFixMessage(t *testing.T) {
	tests := map[string]string{
		"unknown variable": "commit:\n  fix_message: '{{.Unknown}}'\n",
		"escape control":   "commit:\n  fix_message: \"chore:\\u001b {{.Summary}}\"\n",
		"line separator":   "commit:\n  fix_message: \"chore:\\u2028{{.Summary}}\"\n",
		"bidi isolate":     "commit:\n  fix_message: \"chore:\\u2066{{.Summary}}\"\n",
		"zero-width space": "commit:\n  fix_message: \"chore:\\u200b{{.Summary}}\"\n",
	}
	for name, data := range tests {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadRepoFromBytes([]byte(data)); err == nil {
				t.Fatal("LoadRepoFromBytes() accepted an invalid commit.fix_message")
			}
		})
	}
}

func TestMerge_CommitFixMessagePrecedence(t *testing.T) {
	t.Parallel()

	globalSource := "global: {{.Summary}}"
	repoSource := "repo({{.Step}}): {{.Summary}}"
	tests := []struct {
		name   string
		global CommitRaw
		repo   CommitRaw
		want   string
	}{
		{name: "default", want: DefaultFixMessageTemplate},
		{name: "global", global: CommitRaw{FixMessage: &globalSource}, want: globalSource},
		{name: "repo", global: CommitRaw{FixMessage: &globalSource}, repo: CommitRaw{FixMessage: &repoSource}, want: repoSource},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Merge(&GlobalConfig{Commit: tt.global}, &RepoConfig{Commit: tt.repo})
			if cfg.Commit.FixMessage != tt.want {
				t.Fatalf("commit.fix_message = %q, want %q", cfg.Commit.FixMessage, tt.want)
			}
		})
	}
}

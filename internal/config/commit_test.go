package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// testFixMessageEllipsis is the documented cut marker; it is spelled out here
// so these tests fail for a behavior change, not a rename.
const testFixMessageEllipsis = "…"

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

func TestCommitRenderFixMessageForBranch_ExtractsIdentifier(t *testing.T) {
	t.Parallel()

	commit := Commit{
		FixMessage:    "{{.Branch}}: {{.Summary}}",
		BranchPattern: `([A-Z]+-[0-9]+)`,
	}
	got, err := commit.RenderFixMessageForBranch(types.StepLint, "fix failing lint", "refs/heads/feature/PROJ-123-add-widget")
	if err != nil {
		t.Fatal(err)
	}
	if want := "PROJ-123: fix failing lint"; got != want {
		t.Fatalf("RenderFixMessageForBranch() = %q, want %q", got, want)
	}
}

func TestCommitRenderFixMessageForBranch_NoIdentifierFailsClosed(t *testing.T) {
	t.Parallel()

	commit := Commit{
		FixMessage:    "{{.Branch}}: {{.Summary}}",
		BranchPattern: `([A-Z]+-[0-9]+)`,
	}
	if got, err := commit.RenderFixMessageForBranch(types.StepLint, "fix failing lint", "feature/no-issue"); err == nil {
		t.Fatalf("RenderFixMessageForBranch() = %q, want no-identifier error", got)
	}
}

func TestCommitRenderFixMessageForBranch_ReplacesCapturedIdentifier(t *testing.T) {
	t.Parallel()

	commit := Commit{
		FixMessage:        "{{.Branch}}: {{.Summary}}",
		BranchPattern:     `^PROJ/([0-9]+)$`,
		BranchReplacement: "PROJ-${1}",
	}
	got, err := commit.RenderFixMessageForBranch(types.StepLint, "preserve invariants", "refs/heads/PROJ/123")
	if err != nil {
		t.Fatal(err)
	}
	if want := "PROJ-123: preserve invariants"; got != want {
		t.Fatalf("RenderFixMessageForBranch() = %q, want %q", got, want)
	}
}

func TestCommitRenderFixMessage_DefaultRemainsUnchangedWithBranchPattern(t *testing.T) {
	t.Parallel()

	commit := Commit{BranchPattern: `([A-Z]+-[0-9]+)`}
	got, err := commit.RenderFixMessageForBranch(types.StepLint, "fix failing lint", "feature/no-issue")
	if err != nil {
		t.Fatal(err)
	}
	if want := "no-mistakes(lint): fix failing lint"; got != want {
		t.Fatalf("RenderFixMessageForBranch() = %q, want %q", got, want)
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

func TestCommitRenderFixMessage_TruncatesSummaryToSubjectCap(t *testing.T) {
	t.Parallel()

	// The default template adds 20 bytes around the summary, so any summary
	// past 4,076 bytes overflows the 4,096-byte subject cap. The fix round must
	// still commit a bounded subject instead of failing the run and losing the
	// fix commit. 4,090 bytes is the maintainer's repro; 4,096 is the input cap.
	const prefix = "no-mistakes(review): "
	for _, size := range []int{MaxFixMessageSummaryBytes - 6, MaxFixMessageSummaryBytes} {
		summary := strings.Repeat("x", size)
		for name, render := range map[string]func(Commit, string) (string, error){
			"RenderFixMessage": func(c Commit, s string) (string, error) {
				return c.RenderFixMessage(types.StepReview, s)
			},
			"RenderFixMessageForBranch": func(c Commit, s string) (string, error) {
				return c.RenderFixMessageForBranch(types.StepReview, s, "feature/1777")
			},
		} {
			t.Run(fmt.Sprintf("%s/%d-byte summary", name, size), func(t *testing.T) {
				t.Parallel()
				got, err := render(Commit{}, summary)
				if err != nil {
					t.Fatalf("render failed instead of truncating: %v", err)
				}
				if len(got) > maxFixMessageSubjectBytes {
					t.Fatalf("rendered %d bytes, want <= %d", len(got), maxFixMessageSubjectBytes)
				}
				// The cut is marked, and every byte the template does not own is
				// spent on the summary rather than wasted.
				want := prefix + strings.Repeat("x", maxFixMessageSubjectBytes-len(prefix)-len(testFixMessageEllipsis)) + testFixMessageEllipsis
				if got != want {
					t.Fatalf("rendered message = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestCommitRenderFixMessage_TruncationKeepsRunesWhole(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"two-byte runes":  strings.Repeat("é", MaxFixMessageSummaryBytes/2-8),
		"four-byte runes": strings.Repeat("😀", MaxFixMessageSummaryBytes/4-4),
	}
	for name, summary := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := (Commit{}).RenderFixMessage(types.StepReview, summary)
			if err != nil {
				t.Fatalf("render failed instead of truncating: %v", err)
			}
			if len(got) > maxFixMessageSubjectBytes {
				t.Fatalf("rendered %d bytes, want <= %d", len(got), maxFixMessageSubjectBytes)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncation split a rune: %q", got)
			}
			if !strings.HasSuffix(got, testFixMessageEllipsis) {
				t.Fatalf("truncated message %q lacks the ellipsis marker", got)
			}
			wantSummaryBytes := maxFixMessageSubjectBytes - len("no-mistakes(review): ") - len(testFixMessageEllipsis)
			if gotSummary := len(got) - len("no-mistakes(review): ") - len(testFixMessageEllipsis); gotSummary > wantSummaryBytes || gotSummary < wantSummaryBytes-utf8.UTFMax+1 {
				t.Fatalf("kept %d summary bytes, want the largest whole-rune prefix of %d", gotSummary, wantSummaryBytes)
			}
		})
	}
}

func TestCommitRenderFixMessage_ExactFitSummaryIsUnchanged(t *testing.T) {
	t.Parallel()

	const prefix = "no-mistakes(review): "
	summary := strings.Repeat("x", maxFixMessageSubjectBytes-len(prefix))
	got, err := (Commit{}).RenderFixMessage(types.StepReview, summary)
	if err != nil {
		t.Fatal(err)
	}
	if want := prefix + summary; got != want {
		t.Fatalf("rendered message = %q, want the summary unchanged at the cap", got)
	}
}

func TestCommitRenderFixMessage_MultipleSummaryPlaceholdersShareTheCap(t *testing.T) {
	t.Parallel()

	summary := strings.Repeat("x", maxFixMessageSubjectBytes/2+1)
	commit := Commit{FixMessage: "{{.Summary}}{{.Summary}}"}
	got, err := commit.RenderFixMessage(types.StepReview, summary)
	if err != nil {
		t.Fatalf("render failed instead of truncating: %v", err)
	}
	if len(got) > maxFixMessageSubjectBytes {
		t.Fatalf("rendered %d bytes, want <= %d", len(got), maxFixMessageSubjectBytes)
	}
	if cuts := strings.Count(got, testFixMessageEllipsis); cuts != 2 {
		t.Fatalf("rendered message marks %d cuts, want one per {{.Summary}}", cuts)
	}
}

func TestCommitRenderFixMessageForBranch_MinimalSummaryBudget(t *testing.T) {
	t.Parallel()

	for _, summaries := range []int{1, 2} {
		for _, tt := range []struct {
			budget int
			want   string
		}{
			{budget: 3, want: "…"},
			{budget: 2, want: ""},
			{budget: 1, want: ""},
			{budget: 0, want: ""},
		} {
			t.Run(fmt.Sprintf("%d summaries/%d bytes", summaries, tt.budget), func(t *testing.T) {
				t.Parallel()
				branches := 16 - summaries
				literal := strings.Repeat("x", 4096-250*branches-tt.budget*summaries)
				commit := Commit{FixMessage: literal + strings.Repeat("{{.Branch}}", branches) + strings.Repeat("{{.Summary}}", summaries)}
				got, err := commit.RenderFixMessageForBranch(types.StepReview, "abcd", strings.Repeat("b", 250))
				if err != nil {
					t.Fatal(err)
				}
				want := literal + strings.Repeat("b", 250*branches) + strings.Repeat(tt.want, summaries)
				if got != want {
					t.Fatalf("rendered message = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestCommitRenderFixMessage_RejectsSubjectOverflowWithoutASummary(t *testing.T) {
	t.Parallel()

	// Only the agent-provided summary is truncated. Literal template text and
	// the branch stay authoritative, so a template that overflows without any
	// summary still fails instead of silently dropping its own text.
	commit := Commit{FixMessage: strings.Repeat("x", maxFixMessageTemplateBytes) + "{{.Branch}}"}
	if _, err := commit.RenderFixMessageForBranch(types.StepReview, "apply fixes", strings.Repeat("b", maxFixMessageSubjectBytes)); err == nil {
		t.Fatal("RenderFixMessageForBranch() accepted a subject its template text overflows")
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

func TestLoadGlobal_CommitBranchReplacement(t *testing.T) {
	t.Parallel()

	cfg, err := LoadGlobalFromBytes([]byte("commit:\n  branch_pattern: '^PROJ/([0-9]+)$'\n  branch_replacement: 'PROJ-${1}'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commit.BranchPattern == nil || *cfg.Commit.BranchPattern != `^PROJ/([0-9]+)$` {
		t.Fatalf("commit.branch_pattern = %v, want configured pattern", cfg.Commit.BranchPattern)
	}
	if cfg.Commit.BranchReplacement == nil || *cfg.Commit.BranchReplacement != "PROJ-${1}" {
		t.Fatalf("commit.branch_replacement = %v, want configured replacement", cfg.Commit.BranchReplacement)
	}
}

func TestLoadGlobal_RejectsInvalidCommitFixMessage(t *testing.T) {
	tests := map[string]string{
		"unknown variable":                   "commit:\n  fix_message: '{{.Unknown}}'\n",
		"template function":                  "commit:\n  fix_message: '{{printf \"%s\" .Summary}}'\n",
		"conditional":                        "commit:\n  fix_message: '{{if .Summary}}{{.Summary}}{{end}}'\n",
		"named template":                     "commit:\n  fix_message: '{{define \"loop\"}}{{template \"loop\"}}{{end}}{{template \"loop\"}}'\n",
		"malformed syntax":                   "commit:\n  fix_message: '{{'\n",
		"empty template":                     "commit:\n  fix_message: ''\n",
		"oversized template":                 "commit:\n  fix_message: '" + strings.Repeat("x", maxFixMessageTemplateBytes+1) + "'\n",
		"too many placeholders":              "commit:\n  fix_message: '" + strings.Repeat("{{.Summary}}", maxFixMessagePlaceholders+1) + "'\n",
		"multiline output":                   "commit:\n  fix_message: |-\n    first line\n    second line\n",
		"bell control":                       "commit:\n  fix_message: \"chore:\\u0007 {{.Summary}}\"\n",
		"escape control":                     "commit:\n  fix_message: \"chore:\\u001b {{.Summary}}\"\n",
		"line separator":                     "commit:\n  fix_message: \"chore:\\u2028{{.Summary}}\"\n",
		"paragraph separator":                "commit:\n  fix_message: \"chore:\\u2029{{.Summary}}\"\n",
		"bidi override":                      "commit:\n  fix_message: \"chore:\\u202e{{.Summary}}\"\n",
		"zero-width space":                   "commit:\n  fix_message: \"chore:\\u200b{{.Summary}}\"\n",
		"multiple branch captures":           "commit:\n  branch_pattern: '^([A-Z]+)/([0-9]+)$'\n",
		"unbraced capture reference":         "commit:\n  branch_pattern: '^PROJ/([0-9]+)$'\n  branch_replacement: 'PROJ-$1draft'\n",
		"malformed capture reference":        "commit:\n  branch_pattern: '^PROJ/([0-9]+)$'\n  branch_replacement: 'PROJ-${1'\n",
		"wrong capture reference":            "commit:\n  branch_pattern: '^PROJ/([0-9]+)$'\n  branch_replacement: 'PROJ-${2}'\n",
		"multiple capture references":        "commit:\n  branch_pattern: '^PROJ/([0-9]+)$'\n  branch_replacement: '${1}-${1}'\n",
		"literal dollar":                     "commit:\n  branch_pattern: '^PROJ/([0-9]+)$'\n  branch_replacement: '$$${1}'\n",
		"replacement without branch pattern": "commit:\n  branch_replacement: 'PROJ-${1}'\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := LoadGlobal(path); err == nil {
				t.Fatal("LoadGlobal() accepted an invalid commit configuration")
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
		"unknown variable":         "commit:\n  fix_message: '{{.Unknown}}'\n",
		"escape control":           "commit:\n  fix_message: \"chore:\\u001b {{.Summary}}\"\n",
		"line separator":           "commit:\n  fix_message: \"chore:\\u2028{{.Summary}}\"\n",
		"bidi isolate":             "commit:\n  fix_message: \"chore:\\u2066{{.Summary}}\"\n",
		"zero-width space":         "commit:\n  fix_message: \"chore:\\u200b{{.Summary}}\"\n",
		"invalid branch regex":     "commit:\n  branch_pattern: '[['\n",
		"missing branch capture":   "commit:\n  branch_pattern: 'PROJ-[0-9]+'\n",
		"multiple branch captures": "commit:\n  branch_pattern: '([A-Z]+)-([0-9]+)'\n",
	}
	for name, data := range tests {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadRepoFromBytes([]byte(data)); err == nil {
				t.Fatal("LoadRepoFromBytes() accepted an invalid commit configuration")
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
			cfg := Merge(&GlobalConfig{Commit: GlobalCommitRaw{CommitRaw: tt.global}}, &RepoConfig{Commit: tt.repo})
			if cfg.Commit.FixMessage != tt.want {
				t.Fatalf("commit.fix_message = %q, want %q", cfg.Commit.FixMessage, tt.want)
			}
		})
	}
}

func TestMerge_CommitBranchReplacementIsGlobalOnly(t *testing.T) {
	t.Parallel()

	globalPattern := `^PROJ/([0-9]+)$`
	globalReplacement := "PROJ-${1}"
	repoPattern := `([A-Z]+-[0-9]+)`

	t.Run("global replacement applies", func(t *testing.T) {
		cfg := Merge(
			&GlobalConfig{Commit: GlobalCommitRaw{CommitRaw: CommitRaw{BranchPattern: &globalPattern}, BranchReplacement: &globalReplacement}},
			&RepoConfig{},
		)
		got, err := cfg.Commit.BranchValue("PROJ/123")
		if err != nil {
			t.Fatal(err)
		}
		if want := "PROJ-123"; got != want {
			t.Fatalf("BranchValue() = %q, want %q", got, want)
		}
	})

	t.Run("repo pattern disables global replacement", func(t *testing.T) {
		cfg := Merge(
			&GlobalConfig{Commit: GlobalCommitRaw{CommitRaw: CommitRaw{BranchPattern: &globalPattern}, BranchReplacement: &globalReplacement}},
			&RepoConfig{Commit: CommitRaw{BranchPattern: &repoPattern}},
		)
		got, err := cfg.Commit.BranchValue("feature/PROJ-123")
		if err != nil {
			t.Fatal(err)
		}
		if want := "PROJ-123"; got != want {
			t.Fatalf("BranchValue() = %q, want %q", got, want)
		}
	})
}

func TestLoadRepo_CommitBranchReplacementIsInert(t *testing.T) {
	t.Parallel()

	repo, err := LoadRepoFromBytes([]byte("commit:\n  branch_replacement: 'WRONG-${1}'\n"))
	if err != nil {
		t.Fatal(err)
	}
	globalPattern := `^PROJ/([0-9]+)$`
	globalReplacement := "PROJ-${1}"
	cfg := Merge(
		&GlobalConfig{Commit: GlobalCommitRaw{CommitRaw: CommitRaw{BranchPattern: &globalPattern}, BranchReplacement: &globalReplacement}},
		repo,
	)
	got, err := cfg.Commit.BranchValue("PROJ/123")
	if err != nil {
		t.Fatal(err)
	}
	if want := "PROJ-123"; got != want {
		t.Fatalf("BranchValue() = %q, want %q", got, want)
	}
}

func TestCommitRenderTrailers_UsesInvocationIdentity(t *testing.T) {
	t.Parallel()

	commit := Commit{Trailers: []string{
		"Co-Authored-By: no-mistakes {{.Agent}} <noreply@example.com>",
		"Assisted-by: no-mistakes:{{.Agent}}:{{.Model}}",
	}}
	got, err := commit.RenderTrailers(TrailerData{Agent: "codex", Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Co-Authored-By: no-mistakes codex <noreply@example.com>",
		"Assisted-by: no-mistakes:codex:gpt-5.5",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("RenderTrailers() = %q, want %q", got, want)
	}
}

func TestCommitRenderTrailers_ReducesAgentReportedValuesToOneToken(t *testing.T) {
	t.Parallel()

	commit := Commit{Trailers: []string{"Assisted-by: {{.Agent}}:{{.Model}}"}}
	tests := []struct {
		name         string
		agent, model string
		want         string
	}{
		{name: "unreported model", agent: "claude", want: "Assisted-by: claude:unknown"},
		{name: "unreported agent", model: "m1", want: "Assisted-by: unknown:m1"},
		{name: "injected trailer line", agent: "claude", model: "claude-opus-5-5\nSigned-off-by: someone", want: "Assisted-by: claude:claude-opus-5-5"},
		{name: "markup only", agent: "claude", model: "<script>", want: "Assisted-by: claude:script"},
		{name: "nothing usable", agent: "claude", model: "<>", want: "Assisted-by: claude:unknown"},
		{name: "overlong", agent: "claude", model: strings.Repeat("m", 100), want: "Assisted-by: claude:" + strings.Repeat("m", maxTrailerValueBytes)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := commit.RenderTrailers(TrailerData{Agent: tt.agent, Model: tt.model})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("RenderTrailers() = %q, want [%q]", got, tt.want)
			}
		})
	}
}

func TestLoadGlobal_CommitTrailers(t *testing.T) {
	t.Parallel()

	cfg, err := LoadGlobalFromBytes([]byte("commit:\n  trailers:\n    - 'Assisted-by: {{.Agent}}:{{.Model}}'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commit.Trailers == nil || len(*cfg.Commit.Trailers) != 1 || (*cfg.Commit.Trailers)[0] != "Assisted-by: {{.Agent}}:{{.Model}}" {
		t.Fatalf("commit.trailers = %v, want the configured trailer", cfg.Commit.Trailers)
	}
}

func TestLoadGlobal_RejectsInvalidCommitTrailers(t *testing.T) {
	tests := map[string]string{
		"summary placeholder": "commit:\n  trailers: ['Note: {{.Summary}}']\n",
		"step placeholder":    "commit:\n  trailers: ['Note: {{.Step}}']\n",
		"unknown placeholder": "commit:\n  trailers: ['Note: {{.Unknown}}']\n",
		"template function":   "commit:\n  trailers: ['Note: {{printf \"%s\" .Agent}}']\n",
		"conditional":         "commit:\n  trailers: ['Note: {{if .Agent}}x{{end}}']\n",
		"not a trailer":       "commit:\n  trailers: ['assisted by {{.Agent}}']\n",
		"empty value":         "commit:\n  trailers: ['Assisted-by:']\n",
		"agent in key":        "commit:\n  trailers: ['{{.Agent}}-assisted: yes']\n",
		"model in key":        "commit:\n  trailers: ['Model-{{.Model}}: x']\n",
		"agent as key":        "commit:\n  trailers: ['{{.Agent}}: x']\n",
		"worst-case overflow": "commit:\n  trailers: ['" + trailerOfSourceBytes(maxTrailerBytes-maxTrailerValueBytes+len("{{.Model}}")+1) + "']\n",
		"empty entry":         "commit:\n  trailers: ['']\n",
		"multiline":           "commit:\n  trailers:\n    - |-\n      Assisted-by: a\n      Signed-off-by: b\n",
		"escape control":      "commit:\n  trailers: [\"Assisted-by: \\u001b{{.Agent}}\"]\n",
		"bidi override":       "commit:\n  trailers: [\"Assisted-by: \\u202e{{.Agent}}\"]\n",
		"oversized":           "commit:\n  trailers: ['Assisted-by: " + strings.Repeat("x", maxTrailerBytes) + "']\n",
		"too many":            "commit:\n  trailers: [" + strings.TrimSuffix(strings.Repeat("'Assisted-by: x', ", maxTrailers+1), ", ") + "]\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadGlobalFromBytes([]byte(data)); err == nil {
				t.Fatal("LoadGlobalFromBytes() accepted invalid commit.trailers")
			}
			if _, err := LoadRepoFromBytes([]byte(data)); err == nil {
				t.Fatal("LoadRepoFromBytes() accepted invalid commit.trailers")
			}
		})
	}
}

func trailerOfSourceBytes(n int) string {
	const head = "Assisted-by: {{.Model}} "
	return head + strings.Repeat("x", n-len(head))
}

func TestLoadGlobal_AcceptsCommitTrailerThatFitsItsLongestValue(t *testing.T) {
	t.Parallel()

	source := trailerOfSourceBytes(maxTrailerBytes - maxTrailerValueBytes + len("{{.Model}}"))
	data := []byte("commit:\n  trailers: ['" + source + "']\n")
	if _, err := LoadGlobalFromBytes(data); err != nil {
		t.Fatalf("LoadGlobalFromBytes() rejected a trailer that fits the byte cap: %v", err)
	}
	longest := strings.Repeat("m", maxTrailerValueBytes)
	got, err := Commit{Trailers: []string{source}}.RenderTrailers(TrailerData{Agent: "claude", Model: longest})
	if err != nil {
		t.Fatalf("RenderTrailers() failed for an entry that loaded: %v", err)
	}
	if len(got) != 1 || len(got[0]) != maxTrailerBytes {
		t.Fatalf("RenderTrailers() = %d bytes, want exactly %d", len(got[0]), maxTrailerBytes)
	}
}

func TestCommitRenderTrailers_RejectsPlaceholderInKey(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"{{.Agent}}-assisted: yes", "Model-{{.Model}}: x", "{{.Agent}}: x"} {
		if _, err := (Commit{Trailers: []string{source}}).RenderTrailers(TrailerData{Agent: "codex", Model: "gpt"}); err == nil {
			t.Fatalf("RenderTrailers() accepted %q, whose key is not literal", source)
		}
	}
}

func TestMerge_CommitTrailersPrecedence(t *testing.T) {
	t.Parallel()

	globalTrailers := []string{"Assisted-by: global:{{.Agent}}"}
	repoTrailers := []string{"Assisted-by: repo:{{.Agent}}"}
	var none []string
	tests := []struct {
		name   string
		global CommitRaw
		repo   CommitRaw
		want   []string
	}{
		{name: "default", want: nil},
		{name: "global", global: CommitRaw{Trailers: &globalTrailers}, want: globalTrailers},
		{name: "repo replaces global", global: CommitRaw{Trailers: &globalTrailers}, repo: CommitRaw{Trailers: &repoTrailers}, want: repoTrailers},
		{name: "repo empty list clears global", global: CommitRaw{Trailers: &globalTrailers}, repo: CommitRaw{Trailers: &none}, want: nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Merge(&GlobalConfig{Commit: GlobalCommitRaw{CommitRaw: tt.global}}, &RepoConfig{Commit: tt.repo})
			if strings.Join(cfg.Commit.Trailers, "\n") != strings.Join(tt.want, "\n") || len(cfg.Commit.Trailers) != len(tt.want) {
				t.Fatalf("commit.trailers = %q, want %q", cfg.Commit.Trailers, tt.want)
			}
		})
	}
}

func TestMergeForRemote_CommitTrailersRepositoryOverridePrecedence(t *testing.T) {
	t.Parallel()

	const remote = "https://github.com/acme/widget"
	tests := []struct {
		name     string
		override string
		repo     string
		remote   string
		want     []string
	}{
		{name: "override replaces global", override: "['Assisted-by: machine:{{.Agent}}']", remote: remote, want: []string{"Assisted-by: machine:codex"}},
		{name: "override empty list clears global", override: "[]", remote: remote, want: nil},
		{name: "override without trailers keeps global", remote: remote, want: []string{"Assisted-by: global:codex"}},
		{name: "unmatched remote keeps global", override: "['Assisted-by: machine:{{.Agent}}']", remote: "https://github.com/acme/other", want: []string{"Assisted-by: global:codex"}},
		{name: "repo replaces override", override: "['Assisted-by: machine:{{.Agent}}']", repo: "['Assisted-by: repo:{{.Agent}}']", remote: remote, want: []string{"Assisted-by: repo:codex"}},
		{name: "repo empty list clears override", override: "['Assisted-by: machine:{{.Agent}}']", repo: "[]", remote: remote, want: nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			overrideCommit := "      fix_message: 'machine {{.Summary}}'\n"
			if tt.override != "" {
				overrideCommit += "      trailers: " + tt.override + "\n"
			}
			global, err := LoadGlobalFromBytes([]byte("commit:\n  trailers: ['Assisted-by: global:{{.Agent}}']\n" +
				"repository_overrides:\n  https://github.com/acme/widget.git:\n    commit:\n" + overrideCommit))
			if err != nil {
				t.Fatal(err)
			}
			repo := &RepoConfig{}
			if tt.repo != "" {
				if repo, err = LoadRepoFromBytes([]byte("commit:\n  trailers: " + tt.repo + "\n")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := MergeForRemote(global, repo, tt.remote).Commit.RenderTrailers(TrailerData{Agent: "codex", Model: "gpt"})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") || len(got) != len(tt.want) {
				t.Fatalf("rendered commit.trailers = %q, want %q", got, tt.want)
			}
		})
	}
}

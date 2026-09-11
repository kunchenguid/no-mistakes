package steps

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestPRTemplateStructureRequiresH1TextAndOrder(t *testing.T) {
	t.Parallel()
	template := "# Summary\n## Details\n# Validation\n"
	for name, body := range map[string]string{
		"missing":   "# Summary\nDetails",
		"changed":   "# Summary\n# Tests",
		"reordered": "# Validation\n# Summary",
		"demoted":   "## Summary\n# Validation",
		"fenced":    "```markdown\n# Summary\n# Validation\n```",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateTemplateStructure(template, body); err == nil {
				t.Fatal("invalid H1 structure accepted")
			}
		})
	}
	if err := validateTemplateStructure(template, "# Extra\n# Summary\nFilled.\n# Validation\nDone."); err != nil {
		t.Fatal(err)
	}
}

func TestPRTemplateStructureRecognizesOnlyTopLevelATXH1(t *testing.T) {
	t.Parallel()
	text := "# One\n  # Two ###\n#\n#\tTabbed\n## Two hashes\n### Three hashes\n#hashtag\n    # Indented code\n\t# Tab code\n> # Quoted\n- # List\nSetext\n======\n```markdown\n# Fenced\n```\n~~~\n# Tilde fenced\n~~~\n"
	want := []string{"# One", "# Two ###", "#", "#\tTabbed"}
	if got := templateStructureLines(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("H1s = %q, want %q", got, want)
	}
	if err := validateTemplateStructure("## Optional\n### Nested\n- [ ] Choice\n```\n# Example\n```", "Narrative only."); err != nil {
		t.Fatalf("no-H1 template rejected: %v", err)
	}
}

// Representative abbreviated shapes from the pinned Chatwoot, Forem and
// OpenProject compatibility report; fake drafts test acceptance, not model quality.
func TestPRTemplateDraftAllowsSubordinateCompletion(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, template, body string }{
		{"Chatwoot", "# Pull Request Template\n## Description\n## Type of change\nPlease delete options that are not relevant.\n- [ ] Bug fix\n- [ ] New feature\n## Checklist:\n- [ ] Maintainer approval",
			"# Pull Request Template\n## Description\nFix the helper.\n## Type of change\n- [x] Bug fix\n## Checklist:\n- [ ] Maintainer approval"},
		{"Forem", "## What type of PR is this?\n- [ ] Bug Fix\n## Added/updated tests?\n- [ ] Yes\n- [ ] No, and this is why: _please replace this line with details on why tests\n      have not been included_\n### UI accessibility checklist\n- [ ] Keyboard operation\n## [optional] GIF",
			"## What type of PR is this?\n- [x] Bug Fix\n## Added/updated tests?\n- [x] No, and this is why: this is a prose-only correction."},
		{"OpenProject", "# Ticket\n# What are you trying to accomplish?\n## Screenshots\n<!-- Provide before/after screenshots for visual changes; otherwise, remove this section -->\n# What approach did you choose and why?\n# Merge checklist\n- [ ] Tested major browsers",
			"# Ticket\nNo ticket supplied.\n# What are you trying to accomplish?\nCorrect prose; no visual changes.\n# What approach did you choose and why?\nEdit only the incorrect sentence.\n# Merge checklist\n- [ ] Tested major browsers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx, ag, _ := templateTestContext(t)
			ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				for _, rule := range []string{"Only these H1 headings are structurally required", "Make a best effort", "fill all applicable sections", "falsely claim human signoff", "mark human approval checkboxes complete"} {
					if !strings.Contains(opts.Prompt, rule) {
						t.Errorf("missing drafting rule %q", rule)
					}
				}
				data, _ := json.Marshal(prContent{Title: "fix: correct narrative", Body: tc.body})
				return &agent.Result{Output: data}, nil
			}
			got, err := (&PRStep{}).draftTemplateNarrative(sctx, "feature", "main", sctx.Run.BaseSHA, tc.template)
			if err != nil || got.Body != tc.body {
				t.Fatalf("draft = %+v, error %v", got, err)
			}
		})
	}
}

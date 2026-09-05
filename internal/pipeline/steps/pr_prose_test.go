package steps

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRedactPRContent_OperatorAddressPreservesEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, input, want string }{
		{"subject", "fix: Captain: restore reclamation", "fix: restore reclamation"},
		{"risk", "✅ Low: cApTaIn, the change is bounded", "✅ Low: the change is bounded"},
		{"finding", "- ⚠️ Captain, guard stale wakes", "- ⚠️ guard stale wakes"},
		{"sentences", "Tests passed. Captain, checks are complete.", "Tests passed. checks are complete."},
		{"domain", "The captain, crew and ship remain unchanged.", "The captain, crew and ship remain unchanged."},
		{"quoted", `Keep "Captain, ready" and 'Captain: ready' dialogue.`, `Keep "Captain, ready" and 'Captain: ready' dialogue.`},
		{"curly quotes", "Keep “Captain, ready” and ‘Captain: ready’ dialogue.", "Keep “Captain, ready” and ‘Captain: ready’ dialogue."},
		{"escaped quotes", "Keep &#34;Captain, ready&#34; dialogue.", "Keep &#34;Captain, ready&#34; dialogue."},
		{"inline code", "Captain, keep ``Captain: `ready` `` intact", "keep ``Captain: `ready` `` intact"},
		{"fenced code", "````diff\n+Captain: fixture\n```\nCaptain, still code\n````\nCaptain, done", "````diff\n+Captain: fixture\n```\nCaptain, still code\n````\ndone"},
		{"tilde fence", "~~~text\nCaptain, fixture\n~~~\nCaptain, done", "~~~text\nCaptain, fixture\n~~~\ndone"},
		{"unclosed fence", "```text\nCaptain, fixture", "```text\nCaptain, fixture"},
		{"blockquote", "> Captain, quoted\nCaptain, continued quote\n\nCaptain, done", "> Captain, quoted\nCaptain, continued quote\n\ndone"},
		{"indented code", "    Captain: fixture\n\nCaptain, done", "    Captain: fixture\n\ndone"},
		{"html code", "<pre><code>Captain: fixture\nCaptain, code</code></pre>\nCaptain, done", "<pre><code>Captain: fixture\nCaptain, code</code></pre>\ndone"},
		{"html attribute", `<a title="Captain: fixture">link</a>`, `<a title="Captain: fixture">link</a>`},
		{"html prose", "<details>\n<summary>Captain, fix summary</summary>\n\nCaptain, fixed\n</details>", "<details>\n<summary>fix summary</summary>\n\nfixed\n</details>"},
		{"comment", "<!-- Captain: recorded -->\nCaptain, done", "<!-- Captain: recorded -->\ndone"},
		{"unmatched tick in dialogue", "\"Captain, type a ` character.\"\n\nCaptain, done", "\"Captain, type a ` character.\"\n\ndone"},
		{"markup in inline code", "`<code> Captain: literal`\n\nCaptain, done", "`<code> Captain: literal`\n\ndone"},
		{"inline span cannot cross block code", "A lone ` precedes code.\n\n```text\n`Captain: literal\n```\nCaptain, done", "A lone ` precedes code.\n\n```text\n`Captain: literal\n```\ndone"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := redactPRContent(prContent{Body: tt.input}).Body
			if got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRStep_OperatorAddressBeforeTitleInference(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "pr", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"Captain: fix stale wakes","body":"## What Changed\n\n- guard stale wakes"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content.Title != "fix: fix stale wakes" {
		t.Errorf("title = %q", content.Title)
	}
}

func TestRedactPRContent_OperatorAddressBlockquoteBoundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, input, want string }{
		{"reported heading", "> note\n## Captain, next steps", "> note\n## next steps"},
		{"heading and following prose", "> Captain, quoted\n## Captain, next steps\nCaptain: finish cleanup", "> Captain, quoted\n## next steps\nfinish cleanup"},
		{"bullet list", "> Captain, quoted\n- Captain, next steps", "> Captain, quoted\n- next steps"},
		{"ordered list", "> Captain, quoted\n1. Captain, next steps", "> Captain, quoted\n1. next steps"},
		{"fence and following prose", "> note\n```text\nCaptain, literal\n```\nCaptain: finish cleanup", "> note\n```text\nCaptain, literal\n```\nfinish cleanup"},
		{"paragraph continuation", "> Captain, quoted\nCaptain: continued quote", "> Captain, quoted\nCaptain: continued quote"},
		{"explicit quoted blocks", "> Captain, quoted\n> ## Captain, quoted heading\n> - Captain: quoted item", "> Captain, quoted\n> ## Captain, quoted heading\n> - Captain: quoted item"},
		{"noninterrupting number", "> note\n2. Captain, quoted continuation", "> note\n2. Captain, quoted continuation"},
		{"nonheading hash", "> note\n##Captain, quoted continuation", "> note\n##Captain, quoted continuation"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactPRContent(prContent{Body: tt.input}).Body; got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRStep_OperatorAddress(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		for _, fallback := range []bool{false, true} {
			t.Run(string(provider)+map[bool]string{true: "/fallback", false: "/agent"}[fallback], func(t *testing.T) {
				dir, baseSHA, headSHA := setupGitRepo(t)
				quoted := "`Captain: fixture`\n\n> Captain, quoted dialogue\n\n```diff\n+Captain: repository text\n```"
				ag := &mockAgent{name: "pr", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					if fallback {
						return nil, errors.New("draft unavailable")
					}
					raw, err := json.Marshal(prContent{
						Title: "fix(pipeline): Captain, prevent stale wakes",
						Body:  "## What Changed\n\n- Captain: restore automatic reclamation\n- preserve captain selection\n\n" + quoted,
					})
					return &agent.Result{Output: raw}, err
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
				sctx.UserIntent = "Captain, preserve automatic reclamation"
				review := insertCompletedStep(t, sctx, types.StepReview, findingsJSON(t, types.Findings{
					Items:     []types.Finding{{Severity: types.FindingSeverityWarning, File: "captain.go", Line: 2, Description: "Captain, guard stale wakes"}},
					RiskLevel: "low", RiskRationale: "Captain, the changes are well-scoped",
				}), "")
				fix := "Captain: prevent stale wakes"
				if _, err := sctx.DB.InsertStepRound(review.ID, 2, "auto_fix", nil, &fix, 100); err != nil {
					t.Fatal(err)
				}
				insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
					TestingSummary: "Captain, ran focused tests\nCaptain, checked the final diff",
					Tested:         []string{"echo 'Captain: evidence'"},
					Artifacts:      []types.TestArtifact{{Kind: "command-output", Label: "Captain, captured fixture", URL: "https://example.com/fixture.txt", Content: "Captain: recorded output"}},
				}), "")
				content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, provider, 0)
				if err != nil {
					t.Fatal(err)
				}
				if !fallback {
					if content.Title != "fix(pipeline): prevent stale wakes" {
						t.Errorf("title = %q", content.Title)
					}
					for _, want := range []string{quoted, "preserve captain selection", "- restore automatic reclamation"} {
						if !strings.Contains(content.Body, want) {
							t.Errorf("missing %q", want)
						}
					}
				}
				for _, leak := range []string{"Captain, preserve", "Captain, guard", "Captain, the", "Captain: prevent", "Captain, ran", "Captain, checked", "Captain, captured"} {
					if strings.Contains(content.Body, leak) {
						t.Errorf("public prose leaked %q:\n%s", leak, content.Body)
					}
				}
				for _, want := range []string{noMistakesPRSignature, "captain.go", "Captain: evidence", "Captain: recorded output", "guard stale wakes", "ran focused tests", "the changes are well-scoped"} {
					if !strings.Contains(content.Body, want) {
						t.Errorf("missing %q:\n%s", want, content.Body)
					}
				}
				if provider == scm.ProviderGitHub {
					assertFirstAttestationBindsHead(t, content.Body, headSHA)
				}
			})
		}
	}
}

func TestPRStep_OperatorAddressRendering(t *testing.T) {
	t.Parallel()
	type renderCase struct {
		name, body, risk, fix string
		descriptions, tested  []string
		artifacts             []types.TestArtifact
		want                  []string
	}
	cases := []renderCase{
		{
			name: "blockquote followed by heading and list",
			body: "> Captain, quoted evidence\n## Captain, next steps\n- Captain: finish cleanup",
			want: []string{"> Captain, quoted evidence\n## next steps\n- finish cleanup"},
		},
		{
			name:         "markup inside captured output",
			artifacts:    []types.TestArtifact{{Kind: "command-output", Label: "Captured output", Content: "<code>\nCaptain: literal output"}},
			descriptions: []string{"Captain, retain errors"},
			fix:          "Captain: finish cleanup",
			want:         []string{"<code>\nCaptain: literal output", "`captain.go:1` - retain errors\n", "🔧 Fix: finish cleanup"},
		},
		{
			name:         "backtick inside command evidence",
			tested:       []string{"printf 'Captain: literal' # `"},
			descriptions: []string{"Captain, retain errors"},
			fix:          "Captain: finish cleanup",
			want:         []string{"printf 'Captain: literal' # `", "`captain.go:1` - retain errors\n", "🔧 Fix: finish cleanup"},
		},
		{
			name: "unmatched tick inside dialogue",
			body: "\"Captain, type a ` character.\"\n\nCaptain, finish cleanup",
			want: []string{"\"Captain, type a ` character.\"", "\n\nfinish cleanup"},
		},
		{
			name:         "consecutive contractions",
			descriptions: []string{"Don't skip cleanup", "Captain, don't swallow errors"},
			want:         []string{"`captain.go:1` - Don't skip cleanup\n", "`captain.go:2` - don't swallow errors\n"},
		},
		{
			name:         "contraction before command",
			descriptions: []string{"Don't skip cleanup"},
			tested:       []string{"printf 'ready'; echo ready. Captain: `literal`"},
			want:         []string{"printf 'ready'; echo ready. Captain: `literal`"},
		},
		{
			name: "quotes cannot consume code delimiters",
			body: "The fragment ' precedes <code>printf 'ready'; echo ready. Captain: literal</code>.\n\n" +
				"The fragment \" precedes <pre>echo \"ready\". Captain, literal output</pre>.\n\n" +
				"Captain, finish cleanup",
			want: []string{"<code>printf 'ready'; echo ready. Captain: literal</code>", "<pre>echo \"ready\". Captain, literal output</pre>", "\n\nfinish cleanup"},
		},
		{
			name: "contractions inside dialogue",
			descriptions: []string{
				"Keep 'Captain, don't skip cleanup. Captain: retain errors' dialogue.",
				"Keep ‘Captain, don’t skip cleanup. Captain: retain errors’ dialogue.",
				"Captain, preserve captain selection and the captain, crew and ship",
			},
			want: []string{
				"'Captain, don't skip cleanup. Captain: retain errors'",
				"‘Captain, don’t skip cleanup. Captain: retain errors’",
				"`captain.go:3` - preserve captain selection and the captain, crew and ship\n",
			},
		},
	}
	for _, emphasis := range []struct{ prefix, suffix, remaining string }{
		{"**Captain,** ", "", ""},
		{"*cApTaIn:* ", "", ""},
		{"__CAPTAIN,__ ", "", ""},
		{"_Captain:_ ", "", ""},
		{"**Captain**, ", "", ""},
		{"***Captain:*** ", "", ""},
		{"**Captain, ", "**", "**"},
		{"Captain, **", "**", "**"},
	} {
		cases = append(cases, renderCase{
			name:         "emphasis " + emphasis.prefix,
			body:         "Checks passed. " + emphasis.prefix + "finish cleanup" + emphasis.suffix,
			risk:         emphasis.prefix + "the change is bounded" + emphasis.suffix,
			fix:          emphasis.prefix + "guard stale wakes" + emphasis.suffix,
			descriptions: []string{emphasis.prefix + "don't swallow errors" + emphasis.suffix},
			want: []string{
				"Checks passed. " + emphasis.remaining + "finish cleanup" + emphasis.suffix,
				"✅ Low: " + emphasis.remaining + "the change is bounded" + emphasis.suffix,
				"🔧 Fix: " + emphasis.remaining + "guard stale wakes" + emphasis.suffix,
				"`captain.go:1` - " + emphasis.remaining + "don't swallow errors" + emphasis.suffix + "\n",
			},
		})
	}
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		for _, tt := range cases {
			t.Run(string(provider)+"/"+tt.name, func(t *testing.T) {
				dir, baseSHA, headSHA := setupGitRepo(t)
				ag := &mockAgent{name: "pr", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					raw, err := json.Marshal(prContent{Title: "fix(pipeline): preserve rendering", Body: "## What Changed\n\n- guard cleanup\n\n" + tt.body})
					return &agent.Result{Output: raw}, err
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
				findings := types.Findings{RiskLevel: "low", RiskRationale: tt.risk}
				for i, description := range tt.descriptions {
					findings.Items = append(findings.Items, types.Finding{Severity: types.FindingSeverityWarning, File: "captain.go", Line: i + 1, Description: description})
				}
				review := insertCompletedStep(t, sctx, types.StepReview, findingsJSON(t, findings), "")
				if tt.fix != "" {
					if _, err := sctx.DB.InsertStepRound(review.ID, 2, "auto_fix", nil, &tt.fix, 100); err != nil {
						t.Fatal(err)
					}
				}
				insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{Tested: tt.tested, Artifacts: tt.artifacts}), "")
				content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, provider, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range tt.want {
					if !strings.Contains(html.UnescapeString(content.Body), want) {
						t.Errorf("rendered PR missing %q:\n%s", want, content.Body)
					}
				}
				if !strings.Contains(content.Body, noMistakesPRSignature) {
					t.Error("PR signature changed")
				}
				if provider == scm.ProviderGitHub {
					assertFirstAttestationBindsHead(t, content.Body, headSHA)
					steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(content.Body, buildPipelineAttestation(steps, headSHA)) {
						t.Error("pipeline attestation changed during prose sanitization")
					}
				}
			})
		}
	}
}

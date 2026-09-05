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

func TestPRStep_EmptySanitizedTitleUsesFallback(t *testing.T) {
	t.Parallel()
	for _, title := range []string{"Captain:", "**Captain,**", "Captain: "} {
		t.Run(title, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "pr", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				raw, err := json.Marshal(prContent{Title: title, Body: "## What Changed\n\n- guard stale wakes"})
				return &agent.Result{Output: raw}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			if content.Title != "chore: update pull request" {
				t.Errorf("title = %q, want fallback title", content.Title)
			}
			if !strings.Contains(content.Body, "Final changed paths and statuses:") {
				t.Errorf("body is not the fallback diff summary:\n%s", content.Body)
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
				insertCompletedStep(t, sctx, types.StepLint, "", "Captain, lint agent timed out")
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
				for _, leak := range []string{"Captain, preserve", "Captain: restore", "Captain, guard", "Captain, the", "Captain: prevent", "Captain, ran", "Captain, checked", "Captain, captured", "Captain, lint"} {
					if strings.Contains(content.Body, leak) {
						t.Errorf("public prose leaked %q:\n%s", leak, content.Body)
					}
				}
				for _, want := range []string{noMistakesPRSignature, "captain.go", "Captain: evidence", "Captain: recorded output", "guard stale wakes", "ran focused tests", "the changes are well-scoped", "lint agent timed out"} {
					if !strings.Contains(content.Body, want) {
						t.Errorf("missing %q:\n%s", want, content.Body)
					}
				}
				if provider == scm.ProviderGitHub {
					assertFirstAttestationBindsHead(t, content.Body, headSHA)
				}
				rendered, err := json.Marshal(content)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("generated PR: %s", rendered)
			})
		}
	}
}

func TestPRStep_OperatorAddressTestingSummaryFences(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		for _, tt := range []struct{ name, summary string }{
			{"two fences", "Ran game tests:\n```text\nCaptain: ready\n```\nThen ship tests:\n```text\nCaptain: aboard\n```\n\nCaptain, checks passed"},
			{"unclosed fence", "Captain, checks passed\n```text\nCaptain: ready\nCaptain: aboard"},
			{"indented code", "Observed fixture:\n\n    Captain: ready\n    Captain: aboard\n\nCaptain, checks passed"},
			{"tab code", "Observed fixture:\n\n\tCaptain: ready\n\tCaptain: aboard\n\nCaptain, checks passed"},
			{"initial indented code", "    Captain: ready\n    Captain: aboard\n\nCaptain, checks passed"},
		} {
			t.Run(string(provider)+"/"+tt.name, func(t *testing.T) {
				dir, baseSHA, headSHA := setupGitRepo(t)
				ag := &mockAgent{name: "pr", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					raw, err := json.Marshal(prContent{Title: "fix(pipeline): preserve rendering", Body: "## What Changed\n\n- guard cleanup"})
					return &agent.Result{Output: raw}, err
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
				insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{TestingSummary: tt.summary}), "")
				content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, provider, 0)
				if err != nil {
					t.Fatal(err)
				}
				body := html.UnescapeString(strings.ReplaceAll(content.Body, "&#10;", "\n"))
				for _, want := range []string{"Captain: ready", "Captain: aboard", "checks passed"} {
					if !strings.Contains(body, want) {
						t.Errorf("rendered PR missing %q:\n%s", want, content.Body)
					}
				}
				if strings.Contains(body, "Captain, checks") {
					t.Errorf("public prose leaked operator address:\n%s", content.Body)
				}
			})
		}
	}
}

func TestPRStep_OperatorAddressBlockquoteHTML(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, boundary, tail string }{
		{"div", "<div align=\"center\">note</div>", "guard stale wakes"},
		{"table", "<table><tr><td>note</td></tr></table>", "guard stale wakes"},
		{"unclosed div", "<div>", "guard stale wakes"},
		{"comment", "<!-- note -->", "guard stale wakes"},
		{"heading", "## Heading", "guard stale wakes"},
		{"blank before html", "\n<div>note</div>", "guard stale wakes"},
		{"type seven", "<custom-tag>", "Captain, guard stale wakes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			const quoted = "> Captain, quoted dialogue"
			ag := &mockAgent{name: "pr", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				raw, err := json.Marshal(prContent{
					Title: "fix(pipeline): guard stale wakes",
					Body:  "## What Changed\n\n" + quoted + "\n" + tt.boundary + "\nCaptain, guard stale wakes",
				})
				return &agent.Result{Output: raw}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			want := quoted + "\n" + tt.boundary + "\n" + tt.tail
			if !strings.Contains(content.Body, want) {
				t.Errorf("rendered PR missing %q:\n%s", want, content.Body)
			}
			rendered, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("generated PR: %s", rendered)
		})
	}
}

func TestPRStep_OperatorAddressRendering(t *testing.T) {
	t.Parallel()
	type renderCase struct {
		name, intent, body, risk, fix string
		descriptions, tested          []string
		artifacts                     []types.TestArtifact
		want                          []string
	}
	cases := []renderCase{
		{
			name: "type seven inside inline code",
			body: "Keep `literal\n<br>\nCaptain: ready` intact.\n\nCaptain, done",
			want: []string{"Keep `literal\n<br>\nCaptain: ready` intact.\n\ndone"},
		},
		{
			name: "noninterrupting number inside inline code",
			body: "Keep `literal\n2. Captain: ready` intact.\n\nCaptain, done",
			want: []string{"Keep `literal\n2. Captain: ready` intact.\n\ndone"},
		},
		{
			name: "parent code after nested list",
			body: "- outer\n  - inner\n\n  parent paragraph\n\n      Captain: parent code\n\nCaptain, done",
			want: []string{"- outer\n  - inner\n\n  parent paragraph\n\n      Captain: parent code\n\ndone"},
		},
		{
			name:   "intent indented evidence",
			intent: "Observed fixture:\n\n    Captain: ready\n\nCaptain, finish cleanup",
			want:   []string{"## Intent\n\nObserved fixture:\n\nCaptain: ready\n\nfinish cleanup"},
		},
		{
			name:   "intent initial indented evidence",
			intent: "    Captain: ready\n\nCaptain, finish cleanup",
			want:   []string{"## Intent\n\nCaptain: ready\n\nfinish cleanup"},
		},
		{
			name: "inline tick before next paragraph",
			body: "A literal ` character.\n\nCaptain, run `go test`.",
			want: []string{"A literal ` character.\n\nrun `go test`."},
		},
		{
			name:   "unbalanced intent quote before later blocks",
			intent: "Fix the \"flaky test",
			body:   "- Captain, guard wakes",
			risk:   "Captain, bounded. The \"foo\" helper.",
			want:   []string{"Fix the \"flaky test", "- guard cleanup\n\n- guard wakes", "✅ Low: bounded. The \"foo\" helper."},
		},
		{
			name: "inch mark before later blocks",
			body: "- Support 5\" displays\n- Captain, guard wakes",
			risk: "Captain, bounded. The \"foo\" helper.",
			want: []string{"- Support 5\" displays\n- guard wakes", "✅ Low: bounded. The \"foo\" helper."},
		},
		{
			name: "inch mark in ordered list",
			body: "1. Support 5\" displays\n2. Captain, guard wakes. The \"foo\" helper.",
			risk: "Captain, bounded. The \"bar\" helper.",
			want: []string{"1. Support 5\" displays\n2. guard wakes. The \"foo\" helper.", "✅ Low: bounded. The \"bar\" helper."},
		},
		{
			name: "odd quote in heading",
			body: "## Odd \" heading\nCaptain, next \"steps\"",
			risk: "Captain, bounded. The \"bar\" helper.",
			want: []string{"## Odd \" heading\nnext \"steps\"", "✅ Low: bounded. The \"bar\" helper."},
		},
		{
			name: "blockquote followed by thematic break",
			body: "> Captain, quoted evidence\n---\nCaptain, next steps",
			want: []string{"> Captain, quoted evidence\n---\nnext steps"},
		},
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
		{"*Captain:* ", "", ""},
		{"__Captain,__ ", "", ""},
		{"_Captain:_ ", "", ""},
		{"***Captain:*** ", "", ""},
		{"**Captain, ", "**", "**"},
		{"Captain, **", "**", "**"},
		{"**Note:** Captain, ", "", "**Note:** "},
		{"**Captain**, ", "", "**Captain**, "},
		{"*cApTaIn:* ", "", "*cApTaIn:* "},
		{"__CAPTAIN,__ ", "", "__CAPTAIN,__ "},
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
				sctx.UserIntent = tt.intent
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

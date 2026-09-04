package steps

import (
	"context"
	"encoding/json"
	"errors"
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

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
					TestingSummary: "Captain, ran focused tests",
					Tested:         []string{"echo 'Captain: evidence'"},
					Artifacts:      []types.TestArtifact{{Kind: "command-output", Label: "captain fixture", Content: "Captain: recorded output"}},
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
				for _, leak := range []string{"Captain, preserve", "Captain, guard", "Captain, the", "Captain: prevent", "Captain, ran"} {
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

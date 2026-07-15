package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Positive regression for the reported bug: authoritative intent requires a
// pipeline-owned PR outcome ("Open PR A unmerged"). Review runs before push/PR,
// so a finding that only says the PR/remote do not exist yet must not park.
func TestReviewStep_DropsDeferredPipelineOwnedPRFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	reported := `The required criterion says "Open PR A unmerged," but the PR list returned zero PRs and the target commit is not present on a remote branch. PR A still needs to be opened without merging.`

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if !strings.Contains(opts.Prompt, "Pipeline phase (review is pre-push)") {
				t.Errorf("review prompt missing pipeline delivery phase clause:\n%s", opts.Prompt)
			}
			if !strings.Contains(opts.Prompt, "review_scope") {
				t.Errorf("review prompt missing finding scope instructions:\n%s", opts.Prompt)
			}
			if !strings.Contains(string(opts.JSONSchema), `"review_scope"`) {
				t.Errorf("review schema missing review_scope: %s", opts.JSONSchema)
			}
			if !strings.Contains(opts.Prompt, "Do not treat deferred pipeline-owned delivery outcomes") {
				t.Errorf("conformance clause missing deferred-delivery exclusion:\n%s", opts.Prompt)
			}
			if !strings.Contains(opts.Prompt, "Open PR A unmerged") {
				t.Errorf("expected intent body in prompt")
			}
			findings := Findings{
				Items: []Finding{{
					ID:          "intent-missing-pr",
					Severity:    "error",
					Action:      types.ActionAskUser,
					Description: reported,
					ReviewScope: types.FindingReviewScopePipelineOwnedDelivery,
				}},
				Summary:       "missing required open PR",
				RiskLevel:     "high",
				RiskRationale: "required PR criterion not satisfied",
			}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "REQUIRED: Open PR A unmerged."
	sctx.IntentSource = db.RunIntentSourceAgent

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("pipeline-owned missing-PR finding must not require approval; findings=%s", outcome.Findings)
	}
	if hasAskUserFindings(t, outcome.Findings) {
		t.Fatalf("deferred PR finding must be stripped; got %s", outcome.Findings)
	}
	if strings.Contains(outcome.Findings, "PR list returned zero") {
		t.Fatalf("deferred finding text must not survive into outcome: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Error("no remaining findings should mean AutoFixable=false")
	}
}

// Negative: a finding about a pre-existing external PR stays enforceable at
// pre-push review - the strip must not globally ignore external delivery.
func TestReviewStep_KeepsExternalPRLifecycleFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	external := "PR #456 must remain open and unmerged; it is currently closed"

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			findings := Findings{
				Items: []Finding{{
					ID:          "external-pr-closed",
					Severity:    "error",
					Action:      types.ActionAskUser,
					Description: external,
					ReviewScope: types.FindingReviewScopeExternalDelivery,
				}},
				Summary:   "external PR requirement violated",
				RiskLevel: "high",
			}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "REQUIRED: keep PR #456 open and unmerged while shipping this change."
	sctx.IntentSource = db.RunIntentSourceAgent

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("external PR lifecycle finding must still require approval")
	}
	if !hasAskUserFindings(t, outcome.Findings) {
		t.Fatalf("expected ask-user finding preserved; got %s", outcome.Findings)
	}
	if !strings.Contains(outcome.Findings, "PR #456") {
		t.Fatalf("external PR finding text must be preserved: %s", outcome.Findings)
	}
}

// Mixed: real source defect kept; deferred delivery dropped; still parks.
func TestReviewStep_StripsOnlyDeferredAmongMixedFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			findings := Findings{
				Items: []Finding{
					{
						ID:          "deferred-pr",
						Severity:    "error",
						Action:      types.ActionAskUser,
						Description: "PR list returned zero PRs and the target commit is not present on a remote branch",
						ReviewScope: types.FindingReviewScopePipelineOwnedDelivery,
					},
					{
						ID:          "real-bug",
						Severity:    "error",
						Action:      types.ActionAutoFix,
						Description: "nil pointer dereference in handler.go when config is missing",
						ReviewScope: types.FindingReviewScopeSource,
					},
				},
				Summary: "2 issues",
			}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("real source finding must still block")
	}
	if strings.Contains(outcome.Findings, "PR list returned zero") {
		t.Fatalf("deferred finding must be stripped: %s", outcome.Findings)
	}
	if !strings.Contains(outcome.Findings, "nil pointer dereference") {
		t.Fatalf("real finding must be kept: %s", outcome.Findings)
	}
}

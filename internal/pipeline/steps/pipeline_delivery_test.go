package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIsDeferredPipelineOwnedDeliveryFinding(t *testing.T) {
	t.Parallel()

	// Positive: the reported invalid review finding about a PR this run will open.
	reported := `The required criterion says "Open PR A unmerged," but the PR list returned zero PRs and the target commit is not present on a remote branch. PR A still needs to be opened without merging.`

	cases := []struct {
		name  string
		desc  string
		scope string
		want  bool
	}{
		{
			name:  "reported pipeline-owned missing PR",
			desc:  reported,
			scope: types.FindingReviewScopePipelineOwnedDelivery,
			want:  true,
		},
		{
			name:  "remote branch not present yet",
			desc:  "target commit is not present on a remote branch; the branch has not been pushed",
			scope: types.FindingReviewScopePipelineOwnedDelivery,
			want:  true,
		},
		{
			name:  "PR not created yet",
			desc:  "the pull request for this change has not been created yet",
			scope: types.FindingReviewScopePipelineOwnedDelivery,
			want:  true,
		},
		{
			name:  "CI not observed yet",
			desc:  "CI has not run yet for this branch; no checks are present",
			scope: types.FindingReviewScopePipelineOwnedDelivery,
			want:  true,
		},
		// Negative: external / pre-existing lifecycle remains enforceable.
		{
			name:  "numbered external PR must stay open",
			desc:  "PR #456 must remain open and unmerged; it is currently closed",
			scope: types.FindingReviewScopeExternalDelivery,
			want:  false,
		},
		{
			name:  "pre-existing external PR URL missing",
			desc:  "required pre-existing external PR https://github.com/org/dep/pull/99 is missing required approval",
			scope: types.FindingReviewScopeExternalDelivery,
			want:  false,
		},
		{
			name:  "third-party artifact",
			desc:  "required third-party artifact release-notes.pdf is not published",
			scope: types.FindingReviewScopeExternalDelivery,
			want:  false,
		},
		{
			name:  "source implementation bug",
			desc:  "nil pointer dereference in handler.go when config is missing",
			scope: types.FindingReviewScopeSource,
			want:  false,
		},
		{
			name: "proper is not a PR token",
			desc: "there is no proper validation for an empty repository name",
			want: false,
		},
		{
			name: "mixed source and deferred delivery claim",
			desc: "the handler has no proper validation, and the PR for this change is missing",
			want: false,
		},
		{
			name:  "delivery vocabulary inside source defect",
			desc:  "The CI parser panics on malformed responses, and the PR is missing",
			scope: types.FindingReviewScopeSource,
			want:  false,
		},
		{
			name: "missing scope fails closed",
			desc: "the PR for this change is missing",
			want: false,
		},
		{
			name: "intent-required source behavior removed",
			desc: "the fix deletes the intent-required guarded stale-lock removal, leaving rejected retry-only",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isDeferredPipelineOwnedDeliveryFinding(Finding{
				Severity:    "error",
				Action:      types.ActionAskUser,
				Description: tc.desc,
				ReviewScope: tc.scope,
			})
			if got != tc.want {
				t.Errorf("isDeferredPipelineOwnedDeliveryFinding() = %v, want %v\ndesc: %s", got, tc.want, tc.desc)
			}
		})
	}
}

func TestStripDeferredPipelineOwnedDeliveryFindings_Mixed(t *testing.T) {
	t.Parallel()
	in := Findings{
		Items: []Finding{
			{
				ID:          "deferred-pr",
				Severity:    "error",
				Action:      types.ActionAskUser,
				Description: `The required criterion says "Open PR A unmerged," but the PR list returned zero PRs and the target commit is not present on a remote branch. PR A still needs to be opened without merging.`,
				ReviewScope: types.FindingReviewScopePipelineOwnedDelivery,
			},
			{
				ID:          "real-bug",
				Severity:    "error",
				Action:      types.ActionAutoFix,
				Description: "nil pointer dereference in handler.go when config is missing",
				ReviewScope: types.FindingReviewScopeSource,
			},
			{
				ID:          "external-pr",
				Severity:    "error",
				Action:      types.ActionAskUser,
				Description: "PR #456 must remain open and unmerged; it is currently closed",
				ReviewScope: types.FindingReviewScopeExternalDelivery,
			},
		},
		Summary:   "3 issues",
		RiskLevel: "high",
	}
	out, n := stripDeferredPipelineOwnedDeliveryFindings(in)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	if len(out.Items) != 2 {
		t.Fatalf("kept %d items, want 2: %+v", len(out.Items), out.Items)
	}
	ids := map[string]bool{}
	for _, item := range out.Items {
		ids[item.ID] = true
	}
	if ids["deferred-pr"] {
		t.Error("deferred pipeline-owned PR finding should have been stripped")
	}
	if !ids["real-bug"] || !ids["external-pr"] {
		t.Errorf("real and external findings must be kept: %v", ids)
	}
}

func TestStripDeferredPipelineOwnedDeliveryFindings_AllDeferred(t *testing.T) {
	t.Parallel()
	in := Findings{
		Items: []Finding{{
			ID:          "deferred",
			Severity:    "error",
			Action:      types.ActionAskUser,
			Description: "PR list returned zero PRs; the branch is not present on a remote",
			ReviewScope: types.FindingReviewScopePipelineOwnedDelivery,
		}},
		Summary: "missing PR",
	}
	out, n := stripDeferredPipelineOwnedDeliveryFindings(in)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	if len(out.Items) != 0 {
		t.Fatalf("expected empty items, got %+v", out.Items)
	}
	if out.Summary == "missing PR" {
		t.Error("expected summary to note deferred claims were dropped when none remain")
	}
}

func TestPipelineDeliveryPhaseClause_Content(t *testing.T) {
	t.Parallel()
	got := pipelineDeliveryPhaseClause()
	for _, want := range []string{
		"Pipeline phase (review is pre-push)",
		"later pipeline steps",
		"Do NOT emit findings solely because",
		"pre-existing external PR",
		"source-verifiable",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("phase clause missing %q:\n%s", want, got)
		}
	}
}

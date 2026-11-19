package citest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Synthetic Copilot review summaries; no real review text.
const (
	copilotCleanSummary  = "## Pull request overview\n\nAdds a widget.\n\n### Reviewed changes\n\nCopilot reviewed 2 out of 2 changed files in this pull request and generated no comments.\n"
	copilotMissedSummary = "## Needs a closer look\n\n<details><summary>2 previously missed issues</summary>\n<details><summary>Retry loop ignores cancellation</summary>The loop keeps sleeping after the context is done.</details>\n<details><summary>Error text drops the attempt</summary>The wrapped error no longer names the attempt.</details>\n</details>\n\n<details><summary>Resolved since last review (1)</summary>\n\n- An earlier nil check\n</details>\n"
)

// copilotReviewsPayload is the GraphQL answer to the Copilot required-review
// read: the PR at head, whether Copilot is requested, and its reviews.
func copilotReviewsPayload(head string, requested bool, reviews ...map[string]any) string {
	if reviews == nil {
		reviews = []map[string]any{}
	}
	requests := []any{}
	if requested {
		requests = append(requests, map[string]any{"requestedReviewer": map[string]any{"__typename": "Bot", "databaseId": scm.CopilotAuthorID}})
	}
	data, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"number": 42, "headRefOid": head,
		"reviewRequests": map[string]any{"nodes": requests, "pageInfo": map[string]any{"hasNextPage": false}},
		"reviews":        map[string]any{"nodes": reviews, "pageInfo": map[string]any{"hasNextPage": false}},
	}}}})
	return string(data)
}

func copilotSubmittedReview(id, head, submitted, body string) map[string]any {
	return map[string]any{
		"id": "PRR_" + id, "fullDatabaseId": id, "state": "COMMENTED", "body": body,
		"url":         "https://github.com/test/repo/pull/42#pullrequestreview-" + id,
		"author":      map[string]any{"__typename": "Bot", "login": "copilot-pull-request-reviewer", "databaseId": scm.CopilotAuthorID},
		"commit":      map[string]any{"oid": head},
		"submittedAt": submitted, "updatedAt": submitted,
	}
}

func copilotStepContext(t *testing.T, checks, reviews, comments string) (*pipeline.StepContext, string, string) {
	t.Helper()
	dir, base, head := stepstest.SetupGitRepo(t)
	sctx := stepstest.NewTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	sctx.Env = append(stepstest.FakeCIGH(t, "OPEN", checks), "FAKE_CLI_PR_HEAD_SHA="+head, "FAKE_CLI_COPILOT_REVIEWS="+strings.ReplaceAll(reviews, "HEAD_SHA", head), "FAKE_CLI_REVIEW_COMMENTS="+comments)
	sctx.Config.CI.RequiredReviews = []string{"copilot"}
	url := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &url
	return sctx, base, head
}

func TestCIStep_CopilotRequiredReviewReadiness(t *testing.T) {
	t.Parallel()
	const at = "2026-11-19T02:00:00Z"
	for _, policy := range []string{config.CIReviewBotCommentsOnFailure, config.CIReviewBotCommentsAlways} {
		for _, tc := range []struct {
			name         string
			reviews      string
			ready, park  bool
			wantFindings int
		}{
			{name: "clean current-head review", reviews: copilotReviewsPayload("HEAD_SHA", false, copilotSubmittedReview("101", "HEAD_SHA", at, copilotCleanSummary)), ready: true},
			{name: "missing review waits", reviews: copilotReviewsPayload("HEAD_SHA", false)},
			{name: "requested review waits", reviews: copilotReviewsPayload("HEAD_SHA", true)},
			{name: "old head review waits", reviews: copilotReviewsPayload("HEAD_SHA", false, copilotSubmittedReview("101", strings.Repeat("c", 40), at, copilotCleanSummary))},
			{name: "summary-only issues park", reviews: copilotReviewsPayload("HEAD_SHA", false, copilotSubmittedReview("101", "HEAD_SHA", at, copilotMissedSummary)), park: true, wantFindings: 2},
			{name: "assessment concern parks", reviews: copilotReviewsPayload("HEAD_SHA", false, copilotSubmittedReview("101", "HEAD_SHA", at, "## Needs a closer look\n\nConfirm the new cache is meant to be shared.\n\nNo open issues.\n")), park: true, wantFindings: 1},
		} {
			t.Run(policy+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				sctx, base, _ := copilotStepContext(t, `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, tc.reviews, "")
				sctx.Config.CI.ReviewBotComments = policy
				var logs []string
				sctx.Log = func(s string) { logs = append(logs, s) }
				step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true }).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
				outcome, err := step.Execute(sctx)
				if tc.park {
					if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
						t.Fatalf("outcome=%+v err=%v logs=%v", outcome, err, logs)
					}
					findings, err := types.ParseFindingsJSON(outcome.Findings)
					if err != nil {
						t.Fatal(err)
					}
					if len(findings.Items) != tc.wantFindings {
						t.Fatalf("findings=%+v", findings)
					}
					for _, item := range findings.Items {
						if item.Category != types.FindingCategoryCIReviewBot || item.Action != types.ActionAskUser || item.Severity != types.FindingSeverityWarning ||
							item.Check != "External review: copilot" || item.CheckID != "external-review:copilot" || item.File != "" || item.Line != 0 ||
							!strings.Contains(item.Description, "https://github.com/test/repo/pull/42#pullrequestreview-101") {
							t.Fatalf("Copilot summary finding=%+v", item)
						}
					}
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected continued monitoring, outcome=%+v err=%v logs=%v", outcome, err, logs)
				}
				run, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if (run.CIReadyAt != nil) != tc.ready || cimonitor.ChecksPassed(logs) != tc.ready {
					t.Fatalf("readiness=%v logs=%v want=%v", run.CIReadyAt, logs, tc.ready)
				}
			})
		}
	}
}

// A Copilot concern never spends an automatic CI repair; an ordinary failing
// job beside it still does, and every bot keeps its own attribution.
func TestCIStep_CopilotBesideARedJobAndOtherBots(t *testing.T) {
	t.Parallel()
	const at = "2026-11-19T02:00:00Z"
	comments, _ := json.Marshal([]any{
		map[string]any{"id": 501, "author": "copilot-pull-request-reviewer", "author_id": scm.CopilotAuthorID, "path": "internal/widget/a.go", "line": 12, "body": "Unchecked error."},
		map[string]any{"id": 502, "author": "greptile-apps[bot]", "path": "internal/widget/b.go", "line": 3, "body": "Greptile note."},
		map[string]any{"id": 503, "author": "copilot-pull-request-reviewer", "author_id": 1, "path": "internal/widget/c.go", "line": 1, "body": "Spoofed."},
	})
	checks := `[{"name":"build","state":"FAILURE","bucket":"fail"},{"id":300,"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps"}]`
	sctx, base, _ := copilotStepContext(t, checks, copilotReviewsPayload("HEAD_SHA", false, copilotSubmittedReview("101", "HEAD_SHA", at, copilotMissedSummary)), string(comments))
	step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true })
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	var copilot, greptile, jobs int
	for _, item := range findings.Items {
		switch {
		case item.Category == types.FindingCategoryCICheck && item.Check == "build":
			jobs++
			if item.Action != types.ActionAutoFix {
				t.Fatalf("job finding=%+v", item)
			}
		case item.CheckID == "external-review:copilot":
			copilot++
			if item.Action != types.ActionAskUser || strings.Contains(item.Description, "Greptile") || strings.Contains(item.Description, "Spoofed") {
				t.Fatalf("Copilot finding=%+v", item)
			}
		case item.CheckID == "github-check-run:300":
			greptile++
			if strings.Contains(item.Description, "Unchecked error") {
				t.Fatalf("Greptile finding carries Copilot evidence: %+v", item)
			}
		default:
			t.Fatalf("unexpected finding=%+v", item)
		}
	}
	// Two summary-only issues plus the one verified inline thread.
	if jobs != 1 || copilot != 3 || greptile != 1 {
		t.Fatalf("jobs=%d copilot=%d greptile=%d findings=%+v", jobs, copilot, greptile, findings)
	}
}

// With Copilot opted out its threads are never findings, under either
// comment policy, and no Copilot review is read (the fake fails that read).
func TestCIStep_CopilotOptedOutStaysSilent(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{config.CIReviewBotCommentsOnFailure, config.CIReviewBotCommentsAlways} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			comments := `[{"author":"copilot-pull-request-reviewer","author_id":175728472,"path":"a.go","line":1,"body":"Inline note."}]`
			sctx, base, _ := copilotStepContext(t, `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, "", comments)
			sctx.Config.CI.RequiredReviews = nil
			sctx.Config.CI.ReviewBotComments = policy
			var logs []string
			sctx.Log = func(s string) { logs = append(logs, s) }
			step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true }).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
			outcome, err := step.Execute(sctx)
			if !errors.Is(err, context.Canceled) || !cimonitor.ChecksPassed(logs) {
				t.Fatalf("opted-out run did not report ready: outcome=%+v err=%v logs=%v", outcome, err, logs)
			}
		})
	}
}

func TestCIStep_CopilotApprovalReadsLiveEvidence(t *testing.T) {
	t.Parallel()
	const at = "2026-11-19T02:00:00Z"
	other := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, checks, reviews string
		movedHead, allow      bool
	}{
		{name: "missing", checks: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, reviews: copilotReviewsPayload("HEAD_SHA", false)},
		{name: "re-requested", checks: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, reviews: copilotReviewsPayload("HEAD_SHA", true, copilotSubmittedReview("101", "HEAD_SHA", at, copilotCleanSummary))},
		{name: "summary issues", checks: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, reviews: copilotReviewsPayload("HEAD_SHA", false, copilotSubmittedReview("101", "HEAD_SHA", at, copilotMissedSummary))},
		{name: "unreadable", checks: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, reviews: "{"},
		{name: "green review of a different live head", checks: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, movedHead: true,
			reviews: copilotReviewsPayload(other, false, copilotSubmittedReview("101", other, at, copilotCleanSummary))},
		{name: "clean review with an unrelated job failure", checks: `[{"name":"build","state":"FAILURE","bucket":"fail"}]`, allow: true,
			reviews: copilotReviewsPayload("HEAD_SHA", false, copilotSubmittedReview("101", "HEAD_SHA", at, copilotCleanSummary))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, _, _ := copilotStepContext(t, tc.checks, tc.reviews, "")
			if tc.movedHead {
				sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+other)
			}
			reason, err := (&steps.CIStep{}).VerifyApprovalOverride(sctx)
			if tc.allow {
				if err != nil || !strings.Contains(reason, "build") {
					t.Fatalf("reason=%q err=%v", reason, err)
				}
				return
			}
			if !errors.Is(err, pipeline.ErrApprovalBlocked) {
				t.Fatalf("approval not blocked: reason=%q err=%v", reason, err)
			}
		})
	}
}

func TestCIStep_CopilotTimeoutNamesTheReviewer(t *testing.T) {
	t.Parallel()
	sctx, base, _ := copilotStepContext(t, `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, copilotReviewsPayload("HEAD_SHA", true), "")
	sctx.Config.CITimeout = time.Minute
	now := time.Now()
	step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true }).SetNow(func() time.Time { return now }).SetWaitForNextPoll(func(context.Context, time.Duration) error { now = now.Add(2 * time.Minute); return nil })
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || !strings.Contains(outcome.Findings, "copilot is PENDING (CI TIMEOUT)") {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}

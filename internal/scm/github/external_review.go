package github

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GitHub.com GET /apps/coderabbitai identifies its immutable app ID. GHES IDs
// are instance-local and must be explicitly pinned in trusted configuration.
const codeRabbitAppID = scm.CodeRabbitAppID

type reviewState string

const (
	reviewCompletionComplete    reviewState = "complete"
	reviewCompletionMissing     reviewState = "missing"
	reviewCompletionPending     reviewState = "pending"
	reviewCompletionIncomplete  reviewState = "incomplete"
	reviewCompletionRateLimited reviewState = "rate-limited"
	reviewCompletionUnsupported reviewState = "unsupported"
)

type reviewCompletion struct {
	HeadSHA    string
	State      reviewState
	CheckIDs   []string
	AppID      int64
	AuthorID   int64
	CheckNames []string
}

// getExternalReview uses GitHub's latest check runs at an immutable commit,
// independently of ordinary CI's pass/skip normalization. CodeRabbit documents
// both its CodeRabbit check and a passing "Review rate limited" check:
// https://docs.coderabbit.ai/platforms/github-com
// https://docs.coderabbit.ai/management/rate-limits
// No review/comment body is read or used as an instruction or positive proof.
func (h *Host) getExternalReview(ctx context.Context, pr *scm.PR, reviewer string) (reviewCompletion, error) {
	result := reviewCompletion{State: reviewCompletionUnsupported}
	if reviewer != "coderabbit" || h.host == "" {
		return result, nil
	}
	appID := codeRabbitAppID
	if !strings.EqualFold(h.host, "github.com") {
		if h.requiredReviewAppID <= 0 {
			return result, nil
		}
		appID = h.requiredReviewAppID
	}
	selector, err := prSelector(pr)
	if err != nil {
		return result, err
	}
	head := pr.HeadSHA
	decoded, err := hex.DecodeString(head)
	if err != nil || len(decoded) != 20 {
		return result, fmt.Errorf("external review requires a full PR head SHA")
	}
	result.HeadSHA = head
	result.AppID = appID
	result.CheckNames = []string{"CodeRabbit", "Review rate limited"}
	verifyHead := func() error {
		current, err := h.getPRHeadSHA(ctx, selector)
		if err != nil {
			return err
		}
		if current != head {
			return fmt.Errorf("%w during external review discovery", scm.ErrHeadChanged)
		}
		return nil
	}
	if err := verifyHead(); err != nil {
		return result, err
	}
	endpoint := "repos/" + h.repoSlug() + "/commits/" + url.PathEscape(head) + "/check-runs"
	args := []string{"api", "--hostname", h.host, "--method", "GET", endpoint,
		"-f", "app_id=" + strconv.FormatInt(appID, 10), "-f", "filter=latest",
		"-f", "per_page=100", "--paginate", "--slurp"}
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return result, fmt.Errorf("read external review check runs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var pages []struct {
		Total *int `json:"total_count"`
		Runs  []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Head       string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        struct {
				ID   int64  `json:"id"`
				Slug string `json:"slug"`
			} `json:"app"`
			Output struct {
				Title string `json:"title"`
			} `json:"output"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return result, fmt.Errorf("parse external review check runs: %w", err)
	}
	if len(pages) == 0 {
		return result, fmt.Errorf("external review discovery returned no pages")
	}
	result.State = reviewCompletionMissing
	found, pending, incomplete, limited, count := false, false, false, false, 0
	appSlug := ""
	seen := map[int64]bool{}
	for _, page := range pages {
		if page.Total == nil || page.Runs == nil {
			return result, fmt.Errorf("external review discovery returned an incomplete page")
		}
		for _, run := range page.Runs {
			count++
			if run.ID <= 0 || seen[run.ID] || run.App.ID != appID || run.Head != head {
				return result, fmt.Errorf("external review check identity or head could not be verified")
			}
			seen[run.ID] = true
			// Title is only a negative signal. Never promote arbitrary prose,
			// a formal approval, or another check from this app to completion.
			if run.Name == "Review rate limited" || run.Output.Title == "Review rate limited" {
				limited = true
			}
			if run.Name != "CodeRabbit" && run.Name != "Review rate limited" {
				continue
			}
			result.CheckIDs = append(result.CheckIDs, "github-check-run:"+strconv.FormatInt(run.ID, 10))
			if run.Name != "CodeRabbit" {
				continue
			}
			found = true
			if run.App.Slug == "" {
				return result, fmt.Errorf("external review app slug could not be verified")
			}
			appSlug = run.App.Slug
			switch run.Status {
			case "queued", "in_progress", "waiting", "pending", "requested":
				pending = true
			case "completed":
				incomplete = incomplete || run.Conclusion != "success"
			default:
				incomplete = true
			}
		}
	}
	// Pagination races and partial discovery cannot certify a complete review.
	for _, page := range pages {
		if *page.Total != count {
			return result, fmt.Errorf("external review check pagination changed during discovery")
		}
	}
	if err := verifyHead(); err != nil {
		return result, err
	}
	switch {
	case limited:
		result.State = reviewCompletionRateLimited
	case incomplete:
		result.State = reviewCompletionIncomplete
	case pending:
		result.State = reviewCompletionPending
	case found:
		result.State = reviewCompletionComplete
		if !strings.EqualFold(h.host, "github.com") {
			authorOut, err := h.cmd(ctx, "gh", "api", "--hostname", h.host, "users/"+url.PathEscape(appSlug+"[bot]")).CombinedOutput()
			if err != nil {
				return result, fmt.Errorf("resolve instance CodeRabbit user: %s: %w", strings.TrimSpace(string(authorOut)), err)
			}
			var author struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(authorOut, &author); err != nil || author.ID <= 0 {
				return result, fmt.Errorf("resolve instance CodeRabbit user identity")
			}
			result.AuthorID = author.ID
		} else {
			result.AuthorID = scm.CodeRabbitAuthorID
		}
	}
	return result, nil
}

// requiredReviewChecks folds completion and unresolved findings into the same
// check/comment adapter used by Greptile. No separate review-provider service,
// credentials, or findings model is involved.
func (h *Host) requiredReviewChecks(ctx context.Context, pr *scm.PR, reviewer string, checks []scm.Check) ([]scm.Check, error) {
	review, err := h.getExternalReview(ctx, pr, reviewer)
	if err != nil {
		return nil, err
	}
	if review.State == reviewCompletionUnsupported {
		if reviewer == "coderabbit" && h.host != "" && !strings.EqualFold(h.host, "github.com") && h.requiredReviewAppID <= 0 {
			head := pr.ObservedHeadSHA
			if head == "" {
				head = pr.HeadSHA
			}
			return append(checks, scm.Check{
				Name: "External review: coderabbit", ProviderID: "external-review:coderabbit", ExternalReview: "coderabbit",
				HeadSHA: head, Bucket: scm.CheckBucketFail,
				State: "UNSUPPORTED ON GITHUB ENTERPRISE SERVER; configure trusted ci.required_review_app_id",
			}), nil
		}
		return checks, nil
	}
	owned := map[string]bool{}
	for _, id := range review.CheckIDs {
		owned[id] = true
	}
	filtered := make([]scm.Check, 0, len(checks)+1)
	for _, check := range checks {
		if !owned[check.ProviderID] && !(check.AppID == review.AppID && slices.Contains(review.CheckNames, check.Name)) {
			filtered = append(filtered, check)
		}
	}
	sort.Strings(review.CheckIDs)
	check := scm.Check{
		Name: "External review: " + reviewer, ProviderID: "external-review:" + reviewer, ExternalReview: reviewer,
		HeadSHA: review.HeadSHA, ExecutionID: review.HeadSHA + ":" + strings.Join(review.CheckIDs, ","), AppID: review.AppID, AppHost: h.host, App: "coderabbitai",
		Bucket: scm.CheckBucketFail, State: strings.ToUpper(string(review.State)),
	}
	switch review.State {
	case reviewCompletionMissing, reviewCompletionPending:
		check.Bucket = scm.CheckBucketPending
	case reviewCompletionComplete:
		check.Bucket = scm.CheckBucketPass
		comments, err := h.getReviewComments(ctx, pr, review.AuthorID)
		if err != nil {
			return nil, err
		}
		for _, comment := range comments {
			if review.AuthorID > 0 && comment.AuthorID == review.AuthorID {
				check.Bucket = scm.CheckBucketFail
				check.State = "UNRESOLVED_FINDINGS"
				break
			}
		}
	}
	// The comment read belongs to this same immutable snapshot too. A head
	// advance after the check lookup must not authorize its successor.
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	current, err := h.getPRHeadSHA(ctx, selector)
	if err != nil {
		return nil, err
	}
	if current != review.HeadSHA {
		return nil, fmt.Errorf("%w during review findings discovery", scm.ErrHeadChanged)
	}
	return append(filtered, check), nil
}

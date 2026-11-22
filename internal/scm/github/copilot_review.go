package github

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Copilot code review on GitHub.com submits an ordinary pull request review
// as the copilot-pull-request-reviewer bot and publishes no check run, so its
// required-review gate reads the review itself: the latest verified review of
// the PR's current head, its summary body (which can hold issues that have no
// inline comment), and the bot's review threads. It is read-only: it never
// requests a review, replies, or resolves a thread.

// copilotReviewsQuery reads the held pull request's live head, its open review
// requests, and one page of its reviews. Every page repeats the head so a push
// during pagination is detected.
const copilotReviewsQuery = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){number headRefOid reviewRequests(first:100){nodes{requestedReviewer{__typename ... on Bot{databaseId login}}} pageInfo{hasNextPage}} reviews(first:100,after:$cursor){nodes{id fullDatabaseId author{__typename login ... on Bot{databaseId}} commit{oid} submittedAt updatedAt state body url} pageInfo{hasNextPage endCursor}}}}}`

// maxCopilotReviewPages bounds one observation's review pagination. A pull
// request with more reviews than this cannot be read completely, and an
// incomplete read never proves absence, so it is reported as a read failure.
const maxCopilotReviewPages = 20

// maxCopilotFindingBodyBytes bounds one evidence item before the CI findings
// layer applies its own, smaller per-finding budget.
const maxCopilotFindingBodyBytes = 4 * 1024

const copilotReviewerLogin = "copilot-pull-request-reviewer[bot]"

type copilotReviewNode struct {
	ID             string          `json:"id"`
	FullDatabaseID json.RawMessage `json:"fullDatabaseId"`
	Author         *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
		ID       int64  `json:"databaseId"`
	} `json:"author"`
	Commit *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	SubmittedAt *time.Time `json:"submittedAt"`
	UpdatedAt   *time.Time `json:"updatedAt"`
	State       string     `json:"state"`
	Body        *string    `json:"body"`
	URL         string     `json:"url"`

	databaseID string
}

// copilotReviewSnapshot is one complete read of the review requests and every
// review the pinned Copilot identity submitted on the pull request.
type copilotReviewSnapshot struct {
	requested bool
	reviews   []copilotReviewNode
}

type copilotObservation struct {
	State       reviewState
	HeadSHA     string
	Review      *copilotReviewNode
	ReviewState string
	Findings    []scm.ReviewComment
}

// copilotReviewIdentity is what must stay the same between the two reads of
// one observation: the open request and the selected review's version.
func (o copilotObservation) identity() string {
	if o.Review == nil {
		return string(o.State)
	}
	return string(o.State) + ":" + o.Review.ID + ":" + o.Review.UpdatedAt.UTC().Format(time.RFC3339Nano) + ":" + o.Review.State
}

// getCopilotReview reads the Copilot review evidence for the PR head. It is
// supported only on GitHub.com, where the bot's immutable user ID is known.
func (h *Host) getCopilotReview(ctx context.Context, pr *scm.PR) (copilotObservation, error) {
	result := copilotObservation{State: reviewCompletionUnsupported}
	if !strings.EqualFold(h.host, "github.com") {
		return result, nil
	}
	head := pr.HeadSHA
	if decoded, err := hex.DecodeString(head); err != nil || len(decoded) != 20 {
		return result, fmt.Errorf("external review requires a full PR head SHA")
	}
	owner, name, number, err := h.prCoordinates(pr)
	if err != nil {
		return result, err
	}
	first, err := h.readCopilotReviews(ctx, owner, name, number, head)
	if err != nil {
		return result, err
	}
	result, err = h.observeCopilotReview(ctx, pr, owner, name, number, head, first)
	if err != nil {
		return result, err
	}
	// The summary, the threads and the request state are one observation.
	// A newer submission, an edit, a dismissal, or a re-request that lands
	// while they are read must not certify a mixed result, so the review
	// side is read again and must be unchanged.
	second, err := h.readCopilotReviews(ctx, owner, name, number, head)
	if err != nil {
		return result, err
	}
	again, err := selectCopilotReview(second, head)
	if err != nil {
		return result, err
	}
	if again.identity() != selectionIdentity(result) {
		return result, errors.New("Copilot review changed during discovery")
	}
	return result, nil
}

// selectionIdentity drops what only the full observation adds, so it can be
// compared with a bare selection.
func selectionIdentity(o copilotObservation) string {
	switch o.State {
	case reviewCompletionMissing, reviewCompletionPending:
		return copilotObservation{State: o.State, Review: o.Review}.identity()
	}
	return copilotObservation{State: reviewCompletionComplete, Review: o.Review}.identity()
}

// selectCopilotReview picks the latest verified Copilot review of head. An
// older head's review is never a fallback, and an outstanding request for the
// bot means a review is still coming.
func selectCopilotReview(snapshot copilotReviewSnapshot, head string) (copilotObservation, error) {
	result := copilotObservation{State: reviewCompletionMissing, HeadSHA: head}
	var latest *copilotReviewNode
	ambiguous := false
	for i := range snapshot.reviews {
		review := &snapshot.reviews[i]
		if review.Commit == nil || review.Commit.OID != head {
			continue
		}
		if review.State == "PENDING" {
			result.State = reviewCompletionPending
			result.Review = review
			return result, nil
		}
		if latest == nil {
			latest = review
			continue
		}
		switch compareCopilotReviews(review, latest) {
		case 1:
			latest, ambiguous = review, false
		case 0:
			ambiguous = true
		}
	}
	if snapshot.requested {
		result.State = reviewCompletionPending
		return result, nil
	}
	if latest == nil {
		return result, nil
	}
	if ambiguous {
		return result, errors.New("Copilot reviews at the current head cannot be ordered")
	}
	result.State = reviewCompletionComplete
	result.Review = latest
	return result, nil
}

// compareCopilotReviews orders submitted reviews by submission time, then by
// GitHub's review ID. 0 means they cannot be told apart.
func compareCopilotReviews(a, b *copilotReviewNode) int {
	if !a.SubmittedAt.Equal(*b.SubmittedAt) {
		if a.SubmittedAt.After(*b.SubmittedAt) {
			return 1
		}
		return -1
	}
	x, y := a.databaseID, b.databaseID
	if len(x) != len(y) {
		if len(x) > len(y) {
			return 1
		}
		return -1
	}
	return strings.Compare(x, y)
}

func (h *Host) observeCopilotReview(ctx context.Context, pr *scm.PR, owner, name string, number int, head string, snapshot copilotReviewSnapshot) (copilotObservation, error) {
	result, err := selectCopilotReview(snapshot, head)
	if err != nil || result.State != reviewCompletionComplete {
		return result, err
	}
	review := result.Review
	result.ReviewState = review.State
	switch review.State {
	case "COMMENTED", "APPROVED", "CHANGES_REQUESTED":
	default:
		// A dismissed or unknown latest review never lets an older one
		// count again.
		result.State = reviewCompletionIncomplete
		return result, nil
	}
	reviewURLPrefix := "https://github.com/" + owner + "/" + name + "/pull/" + strconv.Itoa(number) + "#pullrequestreview-"
	if !strings.HasPrefix(review.URL, reviewURLPrefix) || review.Body == nil {
		return result, errors.New("Copilot review could not be verified: missing body or review URL")
	}
	threads, err := h.readReviewThreads(ctx, pr, scm.CopilotAuthorID, true)
	if err != nil {
		return result, err
	}
	resolved := map[int64]bool{}
	discussionPrefix := "https://github.com/" + owner + "/" + name + "/pull/" + strconv.Itoa(number) + "#discussion_r"
	for _, comment := range threads {
		bot, ok := scm.ReviewBotForLogin(scm.ProviderGitHub, comment.Author)
		if !ok || bot.RequiredReview != "copilot" || comment.AuthorID != scm.CopilotAuthorID {
			continue
		}
		if id, err := strconv.ParseInt(comment.ID, 10, 64); err == nil {
			resolved[id] = comment.Resolved
		}
		if comment.Resolved {
			continue
		}
		if !strings.HasPrefix(comment.URL, discussionPrefix) {
			comment.URL = ""
		}
		comment.Body = copilotClip(strings.TrimSpace(comment.Body), maxCopilotFindingBodyBytes)
		result.Findings = append(result.Findings, comment.ReviewComment)
	}
	summary := parseCopilotSummary(*review.Body)
	if summary.Incomplete {
		result.State = reviewCompletionIncomplete
	}
	repo := owner + "/" + name
	for i, item := range summary.Items {
		note, keep := copilotSummaryItemDisposition(item, repo, number, resolved)
		if !keep {
			continue
		}
		body := fmt.Sprintf("Review summary %s - %s: %s", item.Section, item.Title, item.Text)
		if note != "" {
			body += " (" + note + ")"
		}
		result.Findings = append(result.Findings, scm.ReviewComment{
			ID:       "copilot-summary:" + review.ID + ":" + strconv.Itoa(i),
			Author:   copilotReviewerLogin,
			AuthorID: scm.CopilotAuthorID,
			Body:     copilotClip(body, maxCopilotFindingBodyBytes),
			URL:      review.URL,
		})
	}
	if review.State == "CHANGES_REQUESTED" && len(result.Findings) == 0 {
		result.Findings = append(result.Findings, scm.ReviewComment{
			ID: "copilot-summary:" + review.ID + ":changes-requested", Author: copilotReviewerLogin, AuthorID: scm.CopilotAuthorID,
			Body: "Copilot requested changes on the current head, but no open issue could be read from the review; inspect it.",
			URL:  review.URL,
		})
	}
	return result, nil
}

// copilotSummaryItemDisposition decides whether a summary item is its own
// finding. An item that only points at verified Copilot threads on this pull
// request is represented by those threads: an open thread is its own finding
// and a resolved one is cleared. A reference that cannot be located, or one to
// another pull request, keeps the item so it is inspected.
func copilotSummaryItemDisposition(item copilotSummaryItem, repo string, number int, resolved map[int64]bool) (string, bool) {
	if len(item.Discussions) == 0 {
		return "", true
	}
	for _, ref := range item.Discussions {
		if ref.CommentID <= 0 || ref.Repo != "" && (!strings.EqualFold(ref.Repo, repo) || ref.Number != number) {
			return "it links a discussion outside this pull request", true
		}
		if _, ok := resolved[ref.CommentID]; !ok {
			return "it links an inline discussion that is not a Copilot review thread on this pull request", true
		}
	}
	return "", false
}

// readCopilotReviews reads every review page. Each page must be for the
// held pull request at the expected head, and only reviews authored by the
// pinned Copilot bot identity are kept: a matching login on another account
// type or ID is ignored.
func (h *Host) readCopilotReviews(ctx context.Context, owner, name string, number int, head string) (copilotReviewSnapshot, error) {
	var snapshot copilotReviewSnapshot
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page == maxCopilotReviewPages {
			return snapshot, fmt.Errorf("Copilot review discovery exceeded %d pages", maxCopilotReviewPages)
		}
		args := []string{"api", "--hostname", h.host, "graphql", "-f", "query=" + copilotReviewsQuery,
			"-F", "owner=" + owner, "-F", "name=" + name, "-F", "number=" + strconv.Itoa(number)}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
		if err != nil {
			return snapshot, fmt.Errorf("read Copilot reviews: %s: %w", strings.TrimSpace(string(out)), err)
		}
		var response struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						Number         int    `json:"number"`
						HeadRefOid     string `json:"headRefOid"`
						ReviewRequests *struct {
							Nodes []struct {
								RequestedReviewer *struct {
									Typename string `json:"__typename"`
									ID       int64  `json:"databaseId"`
								} `json:"requestedReviewer"`
							} `json:"nodes"`
							PageInfo *struct {
								HasNextPage bool `json:"hasNextPage"`
							} `json:"pageInfo"`
						} `json:"reviewRequests"`
						Reviews *struct {
							Nodes    []copilotReviewNode `json:"nodes"`
							PageInfo *struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviews"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(out, &response); err != nil {
			return snapshot, fmt.Errorf("decode Copilot reviews: %w", err)
		}
		if len(response.Errors) > 0 {
			return snapshot, fmt.Errorf("read Copilot reviews: %s", response.Errors[0].Message)
		}
		if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
			return snapshot, errors.New("Copilot reviews response did not contain the pull request")
		}
		pull := response.Data.Repository.PullRequest
		if pull.Number != number {
			return snapshot, errors.New("Copilot reviews response named another pull request")
		}
		if pull.HeadRefOid != head {
			return snapshot, fmt.Errorf("%w during Copilot review discovery", scm.ErrHeadChanged)
		}
		requests := pull.ReviewRequests
		if requests == nil || requests.Nodes == nil || requests.PageInfo == nil {
			return snapshot, errors.New("Copilot reviews response did not contain review requests")
		}
		if requests.PageInfo.HasNextPage {
			return snapshot, errors.New("Copilot review requests could not be read completely")
		}
		for _, request := range requests.Nodes {
			if reviewer := request.RequestedReviewer; reviewer != nil && reviewer.Typename == "Bot" && reviewer.ID == scm.CopilotAuthorID {
				snapshot.requested = true
			}
		}
		reviews := pull.Reviews
		if reviews == nil || reviews.Nodes == nil || reviews.PageInfo == nil {
			return snapshot, errors.New("Copilot reviews response did not contain reviews")
		}
		for _, review := range reviews.Nodes {
			if review.ID == "" || seen[review.ID] {
				return snapshot, errors.New("Copilot reviews response repeated or omitted a review identity")
			}
			seen[review.ID] = true
			if review.Author == nil || review.Author.Typename != "Bot" || review.Author.ID != scm.CopilotAuthorID {
				continue
			}
			id := strings.Trim(string(review.FullDatabaseID), `"`)
			if id == "" || strings.TrimLeft(id, "0123456789") != "" || strings.HasPrefix(id, "0") {
				return snapshot, errors.New("Copilot review identity could not be verified")
			}
			review.databaseID = id
			if review.State == "" || review.UpdatedAt == nil || review.State != "PENDING" && review.SubmittedAt == nil {
				return snapshot, errors.New("Copilot review is missing its state or submission time")
			}
			snapshot.reviews = append(snapshot.reviews, review)
		}
		if !reviews.PageInfo.HasNextPage {
			return snapshot, nil
		}
		if reviews.PageInfo.EndCursor == "" || reviews.PageInfo.EndCursor == cursor {
			return snapshot, errors.New("Copilot reviews response returned an invalid page cursor")
		}
		cursor = reviews.PageInfo.EndCursor
	}
}

// prCoordinates resolves the owner, name and number GraphQL needs for pr.
func (h *Host) prCoordinates(pr *scm.PR) (string, string, int, error) {
	repo := h.repoSlug()
	if repo == "" && pr.URL != "" {
		repo = RepoSlug(pr.URL)
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", 0, fmt.Errorf("resolve GitHub repository for the pull request: invalid repository %q", repo)
	}
	prNum := strings.TrimSpace(pr.Number)
	if prNum == "" {
		number, err := parsePullRequestURL(pr.URL, h.host, repo)
		if err != nil {
			return "", "", 0, err
		}
		prNum = strconv.Itoa(number)
	}
	number, err := strconv.Atoi(prNum)
	if err != nil || number <= 0 {
		return "", "", 0, errors.New("expected positive GitHub pull request number")
	}
	return parts[0], parts[1], number, nil
}

// copilotReviewChecks folds the Copilot observation into one synthesized
// check carrying its verified findings. Ordinary checks are left alone, even
// a look-alike named after Copilot.
func (h *Host) copilotReviewChecks(ctx context.Context, pr *scm.PR, checks []scm.Check) ([]scm.Check, error) {
	if h.host != "" && !strings.EqualFold(h.host, "github.com") {
		head := pr.ObservedHeadSHA
		if head == "" {
			head = pr.HeadSHA
		}
		return append(checks, scm.Check{
			Name: "External review: copilot", ProviderID: "external-review:copilot", ExternalReview: "copilot",
			HeadSHA: head, Bucket: scm.CheckBucketFail,
			State: "UNSUPPORTED ON GITHUB ENTERPRISE SERVER; Copilot reviews are read only on GitHub.com",
		}), nil
	}
	observation, err := h.getCopilotReview(ctx, pr)
	if err != nil {
		return nil, err
	}
	if observation.State == reviewCompletionUnsupported {
		return checks, nil
	}
	check := scm.Check{
		Name: "External review: copilot", ProviderID: "external-review:copilot", ExternalReview: "copilot",
		HeadSHA: observation.HeadSHA, AppHost: h.host, ExecutionID: observation.HeadSHA + ":" + selectionIdentity(observation),
		Bucket: scm.CheckBucketFail, State: strings.ToUpper(string(observation.State)),
	}
	if review := observation.Review; review != nil {
		check.Link = review.URL
		if review.SubmittedAt != nil {
			check.CompletedAt = *review.SubmittedAt
		}
	}
	switch observation.State {
	case reviewCompletionMissing, reviewCompletionPending:
		check.Bucket = scm.CheckBucketPending
	case reviewCompletionComplete:
		check.Bucket = scm.CheckBucketPass
		if len(observation.Findings) > 0 {
			check.Bucket = scm.CheckBucketFail
			check.State = "UNRESOLVED_FINDINGS"
		}
	case reviewCompletionIncomplete:
		if observation.ReviewState != "" && observation.ReviewState != "COMMENTED" && observation.ReviewState != "APPROVED" && observation.ReviewState != "CHANGES_REQUESTED" {
			check.State = observation.ReviewState
		}
	}
	check.ReviewFindings = observation.Findings
	return append(checks, check), nil
}

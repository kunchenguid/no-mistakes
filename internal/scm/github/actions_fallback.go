package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GitHub check evidence has two independent readers behind it. The primary one
// is the GraphQL `statusCheckRollup` on the head commit (what `gh pr checks`
// reads), which is served by the Checks API. Some credentials - notably
// fine-grained tokens - are refused that context with a 403 while still being
// allowed to read the repository's GitHub Actions workflow runs and jobs for
// the same commit. Before this fallback existed a run in that state produced no
// evidence at all: GetChecks returned the read error on every poll, the CI step
// logged a warning, and the run sat there until its idle timeout with nothing
// actionable to show for it.
//
// The fallback is deliberately narrow. The rollup stays the primary source and
// is never second-guessed when it answers. Actions evidence is derived only
// when the primary failure classifies as the rollup itself being unreadable,
// and it is bound to the exact repository, PR, and currently published head.
//
// Nothing here may turn doubt into a pass. Actions can see workflow jobs; it
// cannot see check runs published by other GitHub Apps, so a set of green
// Actions jobs alone is not proof that the commit's whole check set is green.
// That gap is closed from the other side: green is certified only when the
// branch's required-check definition is readable and every required identity
// has exactly one exact-current-head Actions mapping. Anything short of that -
// an unreadable required set, a required identity with no mapping or more than
// one, a run on another commit, a job from a superseded attempt, an incomplete
// listing - is reported as unavailable evidence, never as green. Non-green
// evidence (a failing or still-running job) needs no such certification and is
// returned as-is, because it can only make the pipeline wait or escalate.
var (
	// ErrRollupUnavailable marks a primary check read that failed because the
	// GraphQL status-check rollup / Checks API could not be read with this
	// credential, as opposed to a general command failure. It is the only
	// classification that opens the Actions fallback.
	ErrRollupUnavailable = fmt.Errorf("GitHub status check rollup is unreadable: %w", scm.ErrChecksUnavailable)
	// ErrActionsEvidenceMissing marks Actions evidence that does not exist or
	// cannot be completed: no run for the head commit, a run whose jobs cannot
	// be enumerated, an unreadable required-check definition, or a required
	// check identity with no Actions mapping at the current head.
	ErrActionsEvidenceMissing = fmt.Errorf("GitHub Actions evidence is missing: %w", scm.ErrChecksUnavailable)
	// ErrActionsEvidenceAmbiguous marks Actions evidence that exists more than
	// once for one check identity at the current head, so no single result can
	// be attributed to it.
	ErrActionsEvidenceAmbiguous = fmt.Errorf("GitHub Actions evidence is ambiguous: %w", scm.ErrChecksUnavailable)
	// ErrActionsHeadMismatch marks Actions evidence that belongs to a commit
	// other than the head the run is certifying, including a job left behind by
	// a superseded run attempt.
	ErrActionsHeadMismatch = fmt.Errorf("GitHub Actions evidence does not belong to the commit and attempt under test: %w: %w", scm.ErrHeadChanged, scm.ErrChecksUnavailable)
	// ErrActionsAPIFailure marks an Actions REST read that failed outright, so
	// the fallback produced no evidence either.
	ErrActionsAPIFailure = fmt.Errorf("GitHub Actions API read failed: %w", scm.ErrChecksUnavailable)
)

// rollupUnavailableMarkers are the provider messages that mean "this credential
// may not read the check rollup", rather than "the read itself went wrong".
// They are matched case-insensitively against the combined gh output. The list
// is deliberately specific: a general failure (network, rate limit, malformed
// response) must keep the old behavior of surfacing the error, because the
// fallback's evidence is narrower than the rollup's and must not be reached on
// a failure that a retry would clear.
var rollupUnavailableMarkers = []string{
	"resource not accessible",
	"insufficient scopes",
	"not authorized to read",
	"http 403",
	"403 forbidden",
	"statuscheckrollup",
}

// rollupReadError wraps a primary check-read failure, tagging it with
// ErrRollupUnavailable when the provider's own output says the rollup is not
// readable with this credential.
func rollupReadError(stage, output string, err error) error {
	trimmed := strings.TrimSpace(output)
	if classifyRollupUnavailable(trimmed, err) {
		return fmt.Errorf("%s: %s: %w: %w", stage, trimmed, err, ErrRollupUnavailable)
	}
	if trimmed == "" {
		return fmt.Errorf("%s: %w", stage, err)
	}
	return fmt.Errorf("%s: %s: %w", stage, trimmed, err)
}

func classifyRollupUnavailable(output string, err error) bool {
	haystack := strings.ToLower(output)
	if err != nil {
		haystack += "\n" + strings.ToLower(err.Error())
	}
	for _, marker := range rollupUnavailableMarkers {
		if strings.Contains(haystack, marker) {
			return true
		}
	}
	return false
}

// workflowJob is the subset of an Actions job this backend reads. run_id,
// run_attempt, and head_sha are identity, not decoration: they are what proves
// a job belongs to the run, attempt, and commit under test.
type workflowJob struct {
	ID          int64  `json:"id"`
	RunID       int64  `json:"run_id"`
	RunAttempt  int    `json:"run_attempt"`
	HeadSHA     string `json:"head_sha"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completed_at"`
	HTMLURL     string `json:"html_url"`
}

// getActionsFallbackChecks derives check evidence for headSHA from the Actions
// REST API. It is only reached when the primary rollup read classified as
// unreadable. Every failure mode returns an error wrapping
// scm.ErrChecksUnavailable so the caller can tell "no evidence" from "evidence
// that is not green".
func (h *Host) getActionsFallbackChecks(ctx context.Context, pr *scm.PR, headSHA string) ([]scm.Check, error) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return nil, fmt.Errorf("no head commit to bind Actions evidence to: %w", ErrActionsEvidenceMissing)
	}
	repo := h.repoSlug()
	if repo == "" {
		return nil, fmt.Errorf("no repository to bind Actions evidence to: %w", ErrActionsEvidenceMissing)
	}

	runs, err := h.listWorkflowRunsForHead(ctx, headSHA)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", err, ErrActionsAPIFailure)
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no GitHub Actions run for commit %s: %w", headSHA, ErrActionsEvidenceMissing)
	}

	var checks []scm.Check
	for _, run := range runs {
		if !sameCommit(run.HeadSHA, headSHA) {
			return nil, fmt.Errorf("workflow run %d names commit %q, want %s: %w", run.ID, strings.TrimSpace(run.HeadSHA), headSHA, ErrActionsHeadMismatch)
		}
		if run.RunAttempt <= 0 {
			return nil, fmt.Errorf("workflow run %d reports no run attempt: %w", run.ID, ErrActionsEvidenceMissing)
		}
		jobs, err := h.listWorkflowRunJobs(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, ErrActionsAPIFailure)
		}
		runChecks, err := h.checksForRun(run, jobs, headSHA, repo)
		if err != nil {
			return nil, err
		}
		checks = append(checks, runChecks...)
	}
	if len(checks) == 0 {
		return nil, fmt.Errorf("no GitHub Actions result for commit %s: %w", headSHA, ErrActionsEvidenceMissing)
	}

	// Evidence that is not green needs no certification: it can only make the
	// CI step wait or escalate, never merge. Only a would-be pass has to prove
	// it accounts for the branch's whole required-check set.
	if !actionsEvidenceWouldCertify(checks) {
		return checks, nil
	}
	if err := h.assertRequiredChecksMapped(ctx, pr, checks); err != nil {
		return nil, err
	}
	return checks, nil
}

// checksForRun turns one workflow run's jobs into checks, refusing anything
// that cannot be attributed to the exact commit and attempt under test.
func (h *Host) checksForRun(run workflowRun, jobs []workflowJob, headSHA, repo string) ([]scm.Check, error) {
	if len(jobs) == 0 {
		return h.checksForJoblessRun(run, repo)
	}
	checks := make([]scm.Check, 0, len(jobs))
	for _, job := range jobs {
		if job.RunID != 0 && job.RunID != run.ID {
			return nil, fmt.Errorf("job %d belongs to workflow run %d, want %d: %w", job.ID, job.RunID, run.ID, ErrActionsEvidenceAmbiguous)
		}
		if job.RunAttempt <= 0 {
			return nil, fmt.Errorf("job %d of workflow run %d reports no run attempt: %w", job.ID, run.ID, ErrActionsEvidenceMissing)
		}
		// The listing asks for the latest attempt only. A job from any other
		// attempt is a superseded rerun's leftover; treating it as current
		// would let an old green attempt certify a commit whose current
		// attempt has not finished (or has failed).
		if job.RunAttempt != run.RunAttempt {
			return nil, fmt.Errorf("job %d of workflow run %d is from attempt %d, want %d: %w", job.ID, run.ID, job.RunAttempt, run.RunAttempt, ErrActionsHeadMismatch)
		}
		if strings.TrimSpace(job.HeadSHA) != "" && !sameCommit(job.HeadSHA, headSHA) {
			return nil, fmt.Errorf("job %d of workflow run %d names commit %q, want %s: %w", job.ID, run.ID, strings.TrimSpace(job.HeadSHA), headSHA, ErrActionsHeadMismatch)
		}
		name := strings.TrimSpace(job.Name)
		if name == "" {
			// A nameless job has no check identity, so it can neither be
			// matched against a required check nor reported to the user.
			return nil, fmt.Errorf("job %d of workflow run %d has no name: %w", job.ID, run.ID, ErrActionsEvidenceMissing)
		}
		checks = append(checks, scm.Check{
			Name:        name,
			Bucket:      jobBucket(job),
			State:       jobState(job),
			CompletedAt: parseActionsTime(job.CompletedAt),
			Link:        h.jobLink(job, run, repo),
		})
	}
	return checks, nil
}

// checksForJoblessRun handles a run the jobs endpoint reports nothing for. A
// run that has not started yet legitimately has no jobs, and so does a run the
// provider concluded without running anything (a path filter skipping the whole
// workflow). A run that claims to have succeeded with no readable job is not
// evidence of anything and must not be able to certify the commit.
func (h *Host) checksForJoblessRun(run workflowRun, repo string) ([]scm.Check, error) {
	bucket := normalizeCheckBucket("", run.Conclusion)
	if bucket == "" {
		bucket = normalizeCheckBucket("", run.Status)
	}
	if !strings.EqualFold(strings.TrimSpace(run.Status), "completed") {
		// Still queued or running: no jobs yet is the normal state, and the
		// run itself is the pending evidence. An unrecognized non-terminal
		// status stays pending too - unknown is never green.
		bucket = scm.CheckBucketPending
	} else if bucket == "" || bucket == scm.CheckBucketPass {
		return nil, fmt.Errorf("workflow run %d concluded %q with no readable job: %w", run.ID, strings.TrimSpace(run.Conclusion), ErrActionsEvidenceMissing)
	}
	return []scm.Check{{
		Name:        runCheckName(run),
		Bucket:      bucket,
		State:       runState(run),
		CompletedAt: parseActionsTime(run.UpdatedAt),
		Link:        h.runLink(run, repo),
	}}, nil
}

// listWorkflowRunJobs returns the latest attempt's jobs for one workflow run.
// Like the run listing, the pagination is validated rather than trusted: a
// short listing would silently drop a failing job, which is the one way this
// fallback could manufacture a pass.
func (h *Host) listWorkflowRunJobs(ctx context.Context, runID int64) ([]workflowJob, error) {
	repo := h.repoSlug()
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/actions/runs/%d/jobs", runID)
	if repo != "" {
		endpoint = fmt.Sprintf("repos/%s/actions/runs/%d/jobs", repo, runID)
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint,
		"-f", "filter=latest",
		"-f", "per_page=100",
		"--paginate", "--slurp",
	)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api jobs for workflow run %d: %s: %w", runID, strings.TrimSpace(string(out)), err)
	}
	var pages []struct {
		TotalCount *int          `json:"total_count"`
		Jobs       []workflowJob `json:"jobs"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse jobs for workflow run %d: %w", runID, err)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("job discovery for workflow run %d returned no pages", runID)
	}
	var jobs []workflowJob
	totalCount := -1
	jobIDs := make(map[int64]struct{})
	for pageIndex, page := range pages {
		if page.TotalCount == nil || *page.TotalCount < 0 {
			return nil, fmt.Errorf("job page %d of workflow run %d has no valid total_count", pageIndex+1, runID)
		}
		if totalCount == -1 {
			totalCount = *page.TotalCount
		} else if *page.TotalCount != totalCount {
			return nil, fmt.Errorf("job page %d of workflow run %d total_count is %d, want %d", pageIndex+1, runID, *page.TotalCount, totalCount)
		}
		for _, job := range page.Jobs {
			if job.ID == 0 {
				return nil, fmt.Errorf("job page %d of workflow run %d contains a job without an id", pageIndex+1, runID)
			}
			if _, exists := jobIDs[job.ID]; exists {
				return nil, fmt.Errorf("job id %d of workflow run %d appears more than once", job.ID, runID)
			}
			jobIDs[job.ID] = struct{}{}
			jobs = append(jobs, job)
		}
	}
	if len(jobIDs) != totalCount {
		return nil, fmt.Errorf("job discovery for workflow run %d returned %d unique jobs, want %d", runID, len(jobIDs), totalCount)
	}
	return jobs, nil
}

// assertRequiredChecksMapped is the gate between "every Actions job we could
// see is green" and "this commit is green". Actions cannot see check runs
// published by other GitHub Apps, so the only thing that makes the Actions view
// sufficient is the branch's own required-check definition: when every required
// identity maps to exactly one Actions result at the current head, no unseen
// check run can be required. An unreadable definition, an unprotected branch,
// or a required identity with no unique mapping all leave that gap open, and
// the evidence stays unavailable.
func (h *Host) assertRequiredChecksMapped(ctx context.Context, pr *scm.PR, checks []scm.Check) error {
	baseBranch, err := h.checksBaseBranch(ctx, pr)
	if err != nil {
		return fmt.Errorf("%w: %w", err, ErrActionsEvidenceMissing)
	}
	required, err := h.requiredCheckContexts(ctx, baseBranch)
	if err != nil {
		return fmt.Errorf("%w: %w", err, ErrActionsEvidenceMissing)
	}
	if len(required) == 0 {
		return fmt.Errorf("branch %q declares no required check for commit certification: %w", baseBranch, ErrActionsEvidenceMissing)
	}
	seen := make(map[string]int, len(checks))
	for _, check := range checks {
		seen[check.Name]++
	}
	for _, context := range required {
		switch seen[context] {
		case 1:
		case 0:
			return fmt.Errorf("required check %q has no GitHub Actions result at this commit: %w", context, ErrActionsEvidenceMissing)
		default:
			return fmt.Errorf("required check %q maps to %d GitHub Actions results at this commit: %w", context, seen[context], ErrActionsEvidenceAmbiguous)
		}
	}
	return nil
}

// checksBaseBranch resolves the branch whose protection defines the required
// check set. The PR's own recorded base wins; otherwise it is read from the
// forge, which binds the definition to the configured PR rather than to any
// ambient default.
func (h *Host) checksBaseBranch(ctx context.Context, pr *scm.PR) (string, error) {
	if pr != nil {
		if base := strings.TrimSpace(pr.BaseBranch); base != "" {
			return base, nil
		}
	}
	base, err := h.GetPRBaseBranch(ctx, pr)
	if err != nil {
		return "", err
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("gh pr view returned an empty base branch")
	}
	return base, nil
}

// requiredCheckContexts returns the check identities branch protection requires
// on branch, sorted for deterministic reporting. A branch with no protection
// (or no required checks) returns an empty set, which the caller treats as
// insufficient rather than as permission to certify.
func (h *Host) requiredCheckContexts(ctx context.Context, branch string) ([]string, error) {
	repo := h.repoSlug()
	if repo == "" {
		return nil, errors.New("no repository to read required checks from")
	}
	endpoint := fmt.Sprintf("repos/%s/branches/%s/protection/required_status_checks", repo, branch)
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		body := strings.TrimSpace(string(out))
		if isBranchUnprotected(body) {
			return nil, nil
		}
		return nil, fmt.Errorf("gh api required checks for branch %q: %s: %w", branch, body, err)
	}
	var payload struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("parse required checks for branch %q: %w", branch, err)
	}
	unique := make(map[string]struct{}, len(payload.Contexts)+len(payload.Checks))
	for _, context := range payload.Contexts {
		if trimmed := strings.TrimSpace(context); trimmed != "" {
			unique[trimmed] = struct{}{}
		}
	}
	for _, check := range payload.Checks {
		if trimmed := strings.TrimSpace(check.Context); trimmed != "" {
			unique[trimmed] = struct{}{}
		}
	}
	contexts := make([]string, 0, len(unique))
	for context := range unique {
		contexts = append(contexts, context)
	}
	sort.Strings(contexts)
	return contexts, nil
}

// isBranchUnprotected reports whether GitHub answered the required-checks read
// with its definitive "there is no such protection" 404 rather than with a
// failure. Both answers keep the fallback from certifying, but only the
// definitive one must not be reported as an API failure.
func isBranchUnprotected(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "branch not protected") ||
		strings.Contains(lowered, "required status checks not enabled") ||
		strings.Contains(lowered, "http 404")
}

// actionsEvidenceWouldCertify reports whether this evidence set, taken at face
// value, would let the CI step call the commit green.
func actionsEvidenceWouldCertify(checks []scm.Check) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Bucket != scm.CheckBucketPass && check.Bucket != scm.CheckBucketSkip {
			return false
		}
	}
	return true
}

func jobBucket(job workflowJob) scm.CheckBucket {
	if !strings.EqualFold(strings.TrimSpace(job.Status), "completed") {
		// Queued, in progress, waiting, or a status this version does not
		// recognize: none of them is a conclusion, so none of them is green.
		return scm.CheckBucketPending
	}
	if bucket := normalizeCheckBucket("", job.Conclusion); bucket != "" {
		return bucket
	}
	// A completed job whose conclusion this version cannot classify is not
	// known to have succeeded. Keep it out of the pass bucket.
	return scm.CheckBucketPending
}

func jobState(job workflowJob) string {
	if state := strings.ToUpper(strings.TrimSpace(job.Conclusion)); state != "" {
		return state
	}
	return strings.ToUpper(strings.TrimSpace(job.Status))
}

func runState(run workflowRun) string {
	if state := strings.ToUpper(strings.TrimSpace(run.Conclusion)); state != "" {
		return state
	}
	return strings.ToUpper(strings.TrimSpace(run.Status))
}

func runCheckName(run workflowRun) string {
	if name := strings.TrimSpace(run.Name); name != "" {
		return name
	}
	if name := strings.TrimSpace(run.DisplayName); name != "" {
		return name
	}
	return "GitHub Actions workflow"
}

func (h *Host) jobLink(job workflowJob, run workflowRun, repo string) string {
	if link := strings.TrimSpace(job.HTMLURL); link != "" {
		return link
	}
	return fmt.Sprintf("https://%s/%s/actions/runs/%d/job/%d", h.linkHost(), repo, run.ID, job.ID)
}

func (h *Host) runLink(run workflowRun, repo string) string {
	if link := strings.TrimSpace(run.HTMLURL); link != "" {
		return link
	}
	return fmt.Sprintf("https://%s/%s/actions/runs/%d", h.linkHost(), repo, run.ID)
}

func (h *Host) linkHost() string {
	if host := strings.TrimSpace(h.host); host != "" {
		return host
	}
	return "github.com"
}

func parseActionsTime(raw string) time.Time {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// sameCommit compares two commit identifiers. GitHub always answers with full
// 40-character object names here, so this is an exact comparison; it is
// case-insensitive only because hex case is not part of a commit's identity.
func sameCommit(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// Package gitlab implements scm.Host backed by the glab CLI.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to GitLab through the glab CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	host         string // repo's GitLab hostname; scopes the auth check
	projectPath  string // repo's "group/project" path; enables REST job reads
	draft        bool   // open created MRs as drafts (glab mr create --draft)
}

// New builds a Host. cliAvailable reports whether the glab binary is
// resolvable on the caller's PATH (possibly overridden by env). host is the
// repo's GitLab hostname; when set the availability check is scoped to it via
// --hostname so a stale credential for an unrelated configured glab host cannot
// make this repo look unauthenticated. projectPath is the repo's "group/project"
// path (subgroups allowed); when set, pipeline-job reads go through `glab api`
// (REST), which is branch-independent and works in the daemon's detached-HEAD
// worktree, where `glab ci get` refuses to run without a current branch. Both
// are optional; empty reproduces the legacy unscoped behavior.
func New(cmd CmdFactory, cliAvailable func() bool, host, projectPath string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		host:         strings.TrimSpace(host),
		projectPath:  strings.TrimSpace(projectPath),
	}
}

// NewWithDraft builds a Host that opens created MRs as drafts when draft is
// true (glab mr create --draft). See New for the other parameters.
func NewWithDraft(cmd CmdFactory, cliAvailable func() bool, host, projectPath string, draft bool) *Host {
	h := New(cmd, cliAvailable, host, projectPath)
	h.draft = draft
	return h
}

// ProjectPath extracts the "group/project" path (no host, no trailing .git)
// from a GitLab remote URL. GitLab projects can live under nested subgroups, so
// the full path - not just the last two segments - is returned. It handles
// HTTPS/ssh:// URLs and scp-style SSH (git@host:group/project.git). Returns ""
// when no path can be determined; callers treat that as "unknown" and fall back
// to branch-dependent porcelain.
func ProjectPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var path string
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil {
			path = u.Path
		}
	} else if colon := strings.Index(raw, ":"); colon >= 0 && !isWindowsDrivePath(raw) {
		// scp-style: [user@]host:group/project.git -> group/project. The first
		// ':' separates host from path, so the path is recovered whether or not
		// a "user@" prefix is present (e.g. gitlab.example.com:group/project.git).
		// A Windows drive-letter path (C:\...) carries a colon too, but it is a
		// local filesystem path, not a remote URL, so it is excluded above.
		path = raw[colon+1:]
	}
	path = strings.Trim(path, "/")
	return strings.TrimSuffix(path, ".git")
}

// isWindowsDrivePath reports whether raw begins with a Windows drive specifier
// like "C:\..." or "C:/...". Such a path's drive-letter colon must not be
// mistaken for the host:path separator of scp-style SSH syntax, which would
// otherwise turn a local filesystem path into a spurious "group/project".
func isWindowsDrivePath(raw string) bool {
	if len(raw) < 2 || raw[1] != ':' {
		return false
	}
	c := raw[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	return len(raw) == 2 || raw[2] == '\\' || raw[2] == '/'
}

// pipelineJobsArgs returns the glab invocation that lists a pipeline's jobs.
// With a known project path it uses `glab api` (branch-independent, works in a
// detached-HEAD worktree); otherwise it falls back to `glab ci get`, which
// needs a current branch.
func (h *Host) pipelineJobsArgs(pipelineID int) []string {
	if h.projectPath != "" {
		// --paginate walks every page; a pipeline with more jobs than fit on one
		// page (GitLab defaults to 20 per page) would otherwise silently drop the
		// jobs on later pages and the CI verdict could miss a failed job. glab
		// writes one JSON array per page, so the parser handles concatenated docs.
		return []string{"api", "--paginate", fmt.Sprintf("projects/%s/pipelines/%d/jobs", encodeProjectPath(h.projectPath), pipelineID)}
	}
	return []string{"ci", "get", "--pipeline-id", fmt.Sprintf("%d", pipelineID), "--output", "json", "--with-job-details"}
}

// encodeProjectPath renders a "group/subgroup/project" path as the single
// URL-encoded "group%2Fsubgroup%2Fproject" path parameter GitLab's REST API
// expects. Each segment is escaped defensively and rejoined with %2F so a
// reserved character inside a segment is encoded too, not just the separators.
func encodeProjectPath(projectPath string) string {
	segments := strings.Split(projectPath, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "%2F")
}

// pipelineRouteArgs returns the glab invocation for a paginated pipeline REST
// route ("trigger_jobs", "bridges"). It is only used with a known project path;
// callers fall back to the job-only read when the path is unknown, because the
// route cannot address the project any other way.
func (h *Host) pipelineRouteArgs(pipelineID int, route string) []string {
	return []string{"api", "--paginate", fmt.Sprintf("projects/%s/pipelines/%d/%s", encodeProjectPath(h.projectPath), pipelineID, route)}
}

// mergeRequestStatusChecksArgs returns the glab invocation that lists a merge
// request's external status checks. Like the pipeline routes it needs a known
// project path.
func (h *Host) mergeRequestStatusChecksArgs(mrIID string) []string {
	return []string{"api", "--paginate", fmt.Sprintf("projects/%s/merge_requests/%s/status_checks", encodeProjectPath(h.projectPath), url.PathEscape(mrIID))}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitLab }

// Capabilities declares review comments as well: the discussions API exposes
// the unresolved notes a review bot leaves, and GetReviewComments reads them.
// The check-identity half of the review-bot integration stays GitHub-only -
// GitLab job objects name no publishing application - so a bot is identified
// here by the login it comments as, never by a check.
//
// ClosingReferences is true because GitLab closes the issues named by a
// closing keyword in the merge request description when it merges into the
// default branch, which is what an explicit --closes reference relies on.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{
		MergeableState:    true,
		FailedCheckLogs:   true,
		ReviewComments:    true,
		ClosingReferences: true,
	}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("glab CLI is not installed")
	}
	// Scope the auth check to this repo's host. Unscoped `glab auth status`
	// checks every configured instance and exits non-zero if ANY of them has a
	// stale/expired token, even when this repo's own host is fully
	// authenticated. Passing --hostname keeps an unrelated bad credential from
	// poisoning availability for this repo. When the host is unknown we fall
	// back to the unscoped check (fail-safe: same behavior as before).
	authArgs := []string{"auth", "status"}
	if h.host != "" {
		authArgs = append(authArgs, "--hostname", h.host)
	}
	if err := h.cmd(ctx, "glab", authArgs...).Run(); err != nil {
		return errors.New("glab CLI is not authenticated")
	}
	return nil
}

func parseMergeRequestURL(raw, expectedHost, expectedProject string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return 0, errors.New("expected absolute GitLab merge request URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return 0, errors.New("expected HTTP GitLab merge request URL")
	}
	if expectedHost != "" && !strings.EqualFold(parsed.Hostname(), expectedHost) {
		return 0, fmt.Errorf("URL host %q does not match GitLab host %q", parsed.Hostname(), expectedHost)
	}
	projectSegments, number, ok := mergeRequestURLPath(parsed)
	if !ok {
		return 0, errors.New("expected GitLab /group/project/-/merge_requests/number URL")
	}
	// The project half is checked here rather than inside the shape reader:
	// callers that only want the project path accept the same shape with less
	// validation, while a merge request identity must be unambiguous.
	for _, segment := range projectSegments {
		if segment == "" || segment == "." || segment == ".." {
			return 0, errors.New("expected unambiguous GitLab project path")
		}
	}
	actualProject := strings.Join(projectSegments, "/")
	expectedProject = strings.Trim(strings.TrimSpace(expectedProject), "/")
	if expectedProject != "" && !strings.EqualFold(actualProject, expectedProject) {
		return 0, fmt.Errorf("URL project %q does not match GitLab project %q", actualProject, expectedProject)
	}
	escapedSegments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(escapedSegments) != len(projectSegments)+3 || escapedSegments[len(escapedSegments)-1] != strconv.Itoa(number) {
		return 0, errors.New("expected canonical GitLab merge request number path")
	}
	if parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(trimmed, "#") {
		return 0, errors.New("expected GitLab merge request URL without query or fragment")
	}
	return number, nil
}

// mergeRequestURLPath splits a parsed GitLab merge request URL into its project
// path segments and IID. It owns the "<project>/-/merge_requests/<number>"
// shape so the validated merge request read and the project-path extraction
// cannot drift apart.
func mergeRequestURLPath(parsed *url.URL) ([]string, int, bool) {
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) < 5 || segments[len(segments)-3] != "-" || segments[len(segments)-2] != "merge_requests" {
		return nil, 0, false
	}
	number, err := strconv.Atoi(segments[len(segments)-1])
	if err != nil || number <= 0 {
		return nil, 0, false
	}
	return segments[:len(segments)-3], number, true
}

type mrPayload struct {
	Description         *string `json:"description"`
	IID                 int     `json:"iid"`
	Title               string  `json:"title"`
	WebURL              string  `json:"web_url"`
	URL                 string  `json:"url"`
	State               string  `json:"state"`
	HasConflicts        bool    `json:"has_conflicts"`
	DetailedMergeStatus string  `json:"detailed_merge_status"`
	MergeStatus         string  `json:"merge_status"`
	TargetBranch        string  `json:"target_branch"`
	// SHA is the head commit of the merge request's source branch.
	SHA string `json:"sha"`
	// DiffRefs names the head commit of the merge request's latest diff
	// version. GitLab documents it as populating asynchronously after a push,
	// so it is read only as a fallback for a merge request that does not report
	// SHA yet. The fallback is unit-tested only: the GitLab instances exercised
	// while adding it always reported sha, even for a merge request read 174 ms
	// after creation.
	DiffRefs mrDiffRefs `json:"diff_refs"`
	// HeadPipeline is the raw "head_pipeline" value: the pipeline GitLab
	// currently considers the merge request's own, or JSON null when the merge
	// request has no pipeline. It stays raw so a payload that never names the
	// field can be told apart from an explicit null: only the null is a
	// readable "no checks" answer. The pipeline can also lag a new source
	// commit - GitLab only replaces it once a pipeline matching the newer
	// revision exists - so a reader that trusts it without checking the commit
	// it ran at can grade an old pipeline against a newer head.
	HeadPipeline json.RawMessage `json:"head_pipeline"`
}

type mrDiffRefs struct {
	HeadSHA string `json:"head_sha"`
}

// gitlabHeadPipeline is the subset of the pipeline entity that
// `glab mr view --output json` exposes as head_pipeline: the commit the
// pipeline ran at, the ref it ran for, and the aggregate result the check
// reader reports next to the pipeline's jobs.
type gitlabHeadPipeline struct {
	ID         int    `json:"id"`
	SHA        string `json:"sha"`
	Ref        string `json:"ref"`
	Status     string `json:"status"`
	WebURL     string `json:"web_url"`
	FinishedAt string `json:"finished_at"`
	UpdatedAt  string `json:"updated_at"`
}

// completedAt is the pipeline's own completion time, used for the same re-run
// freshness detection job completions feed. updated_at is the fallback: a
// pipeline is re-created for a new head rather than re-run in place, so the
// update time moves whenever its reported result could have changed.
func (p gitlabHeadPipeline) completedAt() time.Time {
	for _, raw := range []string{p.FinishedAt, p.UpdatedAt} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// awaitingManualJob reports whether the pipeline's own status says a person
// must act before it can finish: GitLab reports `manual` for a pipeline stopped
// at a blocking manual job. It is the aggregate form of the job-level rule, for
// when no job or trigger-job row carries the wait itself.
func (p gitlabHeadPipeline) awaitingManualJob() bool {
	return strings.EqualFold(strings.TrimSpace(p.Status), "manual")
}

// sourceRevision is the source-branch commit the merge request currently points
// at, or "" when neither of the fields that carry it was reported. Both fields
// are read, never assumed: a check read that cannot name the revision it is
// looking at cannot bind its result to one.
func (p mrPayload) sourceRevision() string {
	if sha := strings.TrimSpace(p.SHA); sha != "" {
		return sha
	}
	return strings.TrimSpace(p.DiffRefs.HeadSHA)
}

// headPipeline reads the merge request's "head_pipeline" field. GitLab (through
// glab) sends `"head_pipeline": null` when the merge request has no pipeline:
// that returns nil with no error, and callers read it as no pipeline. Every
// other value without a positive numeric ID is an error - a payload that never
// names the field, a value that is not an object, or an ID that is missing,
// null, or not positive. Reading those as no pipeline would make an unexpected
// response look like an empty pipeline, and a trusted `no_ci: true` would then
// report a false no-CI result.
func (p mrPayload) headPipeline() (*gitlabHeadPipeline, error) {
	if len(p.HeadPipeline) == 0 {
		return nil, errors.New(`response has no "head_pipeline" field`)
	}
	trimmed := bytes.TrimSpace(p.HeadPipeline)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var pipeline gitlabHeadPipeline
	if err := json.Unmarshal(trimmed, &pipeline); err != nil {
		return nil, fmt.Errorf("cannot read head_pipeline %.200s: %w", trimmed, err)
	}
	if pipeline.ID <= 0 {
		return nil, fmt.Errorf("head_pipeline has no positive ID: %.200s", trimmed)
	}
	return &pipeline, nil
}

func (p mrPayload) toPR() *scm.PR {
	url := strings.TrimSpace(p.WebURL)
	if url == "" {
		url = strings.TrimSpace(p.URL)
	}
	pr := &scm.PR{URL: url, BaseBranch: strings.TrimSpace(p.TargetBranch)}
	if p.IID > 0 {
		pr.Number = fmt.Sprintf("%d", p.IID)
	}
	return pr
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"mr", "list", "--source-branch", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--target-branch", base)
	}
	// `glab mr list` returns open MRs by default. Older glab accepted
	// `--state opened`, but glab v1.5x removed it (it now exposes
	// -c/--closed, -M/--merged, -A/--all); passing the unknown flag fails the
	// whole command. Rely on the open-by-default behavior.
	args = append(args, "--output", "json")
	cmd := h.cmd(ctx, "glab", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab mr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, errors.New("parse glab mr list JSON: no JSON found")
	}
	var mrs []mrPayload
	if err := json.Unmarshal(trimmed, &mrs); err != nil {
		return nil, fmt.Errorf("parse glab mr list JSON: %w", err)
	}
	if mrs == nil {
		return nil, errors.New("parse glab mr list JSON: expected array")
	}
	if len(mrs) == 0 {
		return nil, nil
	}
	for i, candidate := range mrs {
		url := strings.TrimSpace(candidate.WebURL)
		if url == "" {
			url = strings.TrimSpace(candidate.URL)
		}
		if url == "" {
			return nil, fmt.Errorf("parse glab mr list JSON: entry %d missing merge request URL", i)
		}
		number, err := parseMergeRequestURL(url, h.host, h.projectPath)
		if err != nil {
			return nil, fmt.Errorf("parse glab mr list JSON: entry %d invalid merge request URL: %w", i, err)
		}
		if candidate.IID != 0 && candidate.IID != number {
			return nil, fmt.Errorf("parse glab mr list JSON: entry %d IID %d does not match URL number %d", i, candidate.IID, number)
		}
	}
	pr := mrs[0].toPR()
	return pr, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	effectiveTitle := content.Title
	if h.draft && !isDraftTitle(effectiveTitle) {
		effectiveTitle = "Draft: " + effectiveTitle
	}
	if err := validateMRTitle(effectiveTitle); err != nil {
		return nil, fmt.Errorf("glab mr create: %w", err)
	}
	args := []string{"mr", "create",
		"--source-branch", branch,
		"--target-branch", base,
		"--title", content.Title,
		"--description", content.Body,
		"--yes",
	}
	if h.draft {
		args = append(args, "--draft")
	}
	cmd := h.cmd(ctx, "glab", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab mr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	url := extractMRURL(out)
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := pr.Number
	if id == "" && pr != nil {
		if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
			id = num
		}
	}
	if id == "" && pr != nil {
		id = pr.URL
	}
	// Unlike `glab mr create`, `glab mr update` (glab v1.5x) has no
	// -y/--yes confirmation-skip flag at all; passing it fails the whole
	// command with "unknown flag: --yes", so every UpdatePR call errored.
	//
	// GitLab has no separate draft field: an MR is a draft because its title
	// carries a draft marker. Updating with a plain title would silently mark a
	// draft MR ready for review, so read the live title first and re-apply the
	// marker. Preserve only: a non-draft MR never gains one. A failed read fails
	// the update closed rather than risk toggling draft state.
	args := []string{"mr", "update", id}
	// Body-only updates must omit title, not read then resend it: doing so
	// would overwrite a concurrent title/draft edit.
	if content.Title != "" {
		mr, err := h.viewMR(ctx, id)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(mr.Title) == "" {
			return nil, errors.New("glab mr view: missing merge request title")
		}
		title := content.Title
		if isDraftTitle(mr.Title) && !isDraftTitle(title) {
			title = "Draft: " + title
		}
		if err := validateMRTitle(title); err != nil {
			return nil, fmt.Errorf("glab mr update: %w", err)
		}
		args = append(args, "--title", title)
	}
	args = append(args, "--description", content.Body)
	cmd := h.cmd(ctx, "glab", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("glab mr update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) SetPRBaseBranch(ctx context.Context, pr *scm.PR, baseBranch string) error {
	id := ""
	if pr != nil {
		id = pr.Number
		if id == "" {
			if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
				id = num
			}
		}
		if id == "" {
			id = pr.URL
		}
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("merge request identity is required to retarget")
	}
	cmd := h.cmd(ctx, "glab", "mr", "update", id, "--target-branch", baseBranch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("glab mr update --target-branch: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// isDraftTitle reports whether an MR title carries a marker GitLab treats as
// draft: "Draft:", "[Draft]", or "(Draft)", all case-insensitive.
func isDraftTitle(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	return strings.HasPrefix(t, "draft:") || strings.HasPrefix(t, "[draft]") || strings.HasPrefix(t, "(draft)")
}

func validateMRTitle(title string) error {
	if utf8.RuneCountInString(title) > 255 {
		return errors.New("GitLab merge request title must not exceed 255 characters")
	}
	return nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	return normalizePRState(mr.State), nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	if mr.HasConflicts {
		return scm.MergeableConflict, nil
	}
	// detailed_merge_status is preferred; merge_status is the legacy field.
	status := strings.ToLower(strings.TrimSpace(mr.DetailedMergeStatus))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(mr.MergeStatus))
	}
	switch status {
	case "mergeable", "can_be_merged":
		return scm.MergeableOK, nil
	case "broken_status", "cannot_be_merged":
		return scm.MergeableConflict, nil
	case "checking", "unchecked", "ci_still_running", "":
		return scm.MergeablePending, nil
	default:
		return scm.MergeableOK, nil
	}
}

// viewMR reads the live merge request. Every caller that has to reason about
// the source revision a pipeline belongs to goes through it, so the identity
// fields are parsed in one place.
func (h *Host) viewMR(ctx context.Context, id string) (mrPayload, error) {
	cmd := h.cmd(ctx, "glab", "mr", "view", id, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return mrPayload{}, fmt.Errorf("glab mr view: %s: %w", strings.TrimSpace(string(out)), err)
	}
	mr, ok := parseMRPayload(out)
	if !ok {
		return mrPayload{}, fmt.Errorf("glab mr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return mr, nil
}

// GetChecks reads the merge request's head pipeline the way the GitLab UI
// presents it, not just its job list: a pipeline's result is an aggregate of
// ordinary jobs, trigger jobs (GitLab's bridge jobs, which carry a downstream
// pipeline's result) and, on instances that license the feature, external
// status checks. Reading only `/jobs` lets a nonempty list of green ordinary
// jobs hide a failed or pending pipeline - a trigger stage that failed, or a
// pipeline that failed before it created any job at all.
//
// An instance that does not serve a feature (external status checks are
// Ultimate) reports that as an unavailable route and is read as having none,
// never as an unreadable check; any other failure is a read failure and stays
// one, because "unreadable" must not look green either.
func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	pr.ObservedHeadSHA = ""
	if headSHA := strings.TrimSpace(pr.HeadSHA); headSHA != "" {
		return h.getChecksForHead(ctx, pr, headSHA)
	}
	// Without a named source commit there is nothing to bind a read to, so the
	// pipeline is taken as the merge request reports it. The observation is
	// still composed the same way: a head without a bound commit must not hide
	// a failed or pending pipeline either.
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	pipeline, err := mr.headPipeline()
	if err != nil {
		return nil, fmt.Errorf("glab mr view: %w", err)
	}
	return h.pipelineChecks(ctx, pr, pipeline, nil)
}

// pipelineChecks composes the checks of a head pipeline: its ordinary jobs, its
// trigger jobs, the pipeline's own aggregate status when no job or trigger job
// already carries it, and the merge request's external status checks. A nil
// pipeline means the merge request has no pipeline at all; external status
// checks are keyed on the merge request rather than the pipeline, so they are
// read even then.
//
// afterReads runs once every provider read is done, when the caller has to
// re-establish that the whole observation still describes the same commit (the
// bound path's stability check); its error is returned before any checks are,
// so no part of a moved observation is graded.
func (h *Host) pipelineChecks(ctx context.Context, pr *scm.PR, pipeline *gitlabHeadPipeline, afterReads func() error) ([]scm.Check, error) {
	var checks []scm.Check
	if pipeline != nil {
		jobsOut, err := h.readPipelineJobs(ctx, pipeline.ID)
		if err != nil {
			return nil, err
		}
		jobs, err := parseGitlabJobs(jobsOut)
		if err != nil {
			return nil, err
		}
		checks = jobs
		if h.projectPath != "" {
			// A trigger job is not a `/jobs` row: GitLab exposes it only on its
			// own route, and its status is where a downstream pipeline's result
			// reaches the head pipeline's aggregate.
			triggerJobs, err := h.pipelineTriggerJobs(ctx, pipeline.ID)
			if err != nil {
				return nil, err
			}
			checks = append(checks, triggerJobsToChecks(triggerJobs)...)
		}
		// The aggregate is compared against the pipeline's own rows only: a
		// failed external status check is a separate requirement, so letting it
		// stand in for the pipeline's state would hide a pipeline failure no job
		// row explains (a rejected configuration, most often).
		if aggregate, ok := pipelineAggregateCheck(*pipeline, checks); ok {
			checks = append(checks, aggregate)
		}
	}
	if h.projectPath != "" {
		external, err := h.mergeRequestStatusChecks(ctx, pr.Number)
		if err != nil {
			return nil, err
		}
		if pipeline != nil || hasNonPassingCheck(external) {
			checks = append(checks, external...)
		}
	}
	if afterReads != nil {
		if err := afterReads(); err != nil {
			return nil, err
		}
	}
	return checks, nil
}

// hasNonPassingCheck reports whether any check is not a plain pass. It decides
// whether a merge request with no head pipeline has an external requirement
// worth reporting: passing checks alone must not read as an all-green head,
// because the monitor would stop there and tell the user to merge while the
// pipeline that has not registered yet could still fail. With nothing but
// passes (or nothing at all) the caller returns no checks, and the CI step's
// zero-check rule decides - wait for registration, or pass under a trusted
// `no_ci: true`.
func hasNonPassingCheck(checks []scm.Check) bool {
	for _, check := range checks {
		if check.Bucket != scm.CheckBucketPass {
			return true
		}
	}
	return false
}

// getChecksForHead reads the merge request's checks bound to headSHA, the exact
// source commit the caller is delivering.
//
// GitLab's head pipeline is a pipeline object, not a per-commit check rollup:
// GitLab only replaces it once a pipeline matching a newer source revision
// exists, so the pointer can still name a pipeline an older commit ran at while
// the merge request has already moved on. Reading its jobs unconditionally
// would grade that older commit's result as if it belonged to headSHA. This
// path therefore proves, before reading any job, that the merge request is at
// headSHA and that its head pipeline is the pipeline for headSHA - either it
// ran at that commit, or it is a merged-results/merge-train pipeline whose
// temporary commit includes it (mirroring GitLab's own head-pipeline check; see
// mergeResultIncludes). It then re-reads the merge request once every provider
// read is done and rejects a source revision that moved mid-observation,
// exactly as the GitHub adapter does. Everything the observation carries -
// jobs, trigger jobs, aggregate, external status checks - is built from this
// proven pipeline and merge request, so no part of it can describe another
// commit.
func (h *Host) getChecksForHead(ctx context.Context, pr *scm.PR, headSHA string) ([]scm.Check, error) {
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	pr.ObservedHeadSHA = mr.sourceRevision()
	pipeline, err := h.headPipelineBoundTo(ctx, pr, mr, headSHA)
	if err != nil {
		return nil, err
	}
	// Everything the observation carries - the job list, the trigger jobs, the
	// merge request's external status checks and the pipeline's aggregate - is
	// read from this proven pipeline and merge request. The stability re-read
	// runs after every provider read, so a source revision that moved
	// mid-observation rejects the whole set.
	after := func() error {
		afterMR, err := h.viewMR(ctx, pr.Number)
		if err != nil {
			return err
		}
		if revision := afterMR.sourceRevision(); revision != "" {
			pr.ObservedHeadSHA = revision
		}
		if revision := afterMR.sourceRevision(); revision == "" {
			return fmt.Errorf("merge request %s reported no source commit", pr.Number)
		} else if !sameCommitSHA(revision, headSHA) {
			return fmt.Errorf("%w: merge request %s source commit changed during check discovery from %s to %s", scm.ErrHeadChanged, pr.Number, headSHA, revision)
		}
		return nil
	}
	// A nil pipeline means no job has registered for this commit; the merge
	// request's external status checks are still read (and still bound: the
	// source revision above already proved the merge request is at headSHA).
	return h.pipelineChecks(ctx, pr, pipeline, after)
}

// headPipelineBoundTo resolves the merge request's head pipeline and proves it
// belongs to headSHA. It returns a nil pipeline (and no error) only when the
// merge request reports no head pipeline at all.
//
// Every refusal that means "the evidence in hand is not evidence for the
// delivered commit" wraps scm.ErrHeadChanged, so a caller can tell it from a
// provider or CLI read failure and wait for the pipeline instead of treating
// the state as a broken integration. Failures that mean the response could not
// be read or could not be checked at all - an omitted source commit or pipeline
// commit, an unknown project path - stay plain errors.
func (h *Host) headPipelineBoundTo(ctx context.Context, pr *scm.PR, mr mrPayload, headSHA string) (*gitlabHeadPipeline, error) {
	revision := mr.sourceRevision()
	if revision == "" {
		// A read that cannot name the source revision cannot bind a pipeline to
		// it; failing closed keeps an unidentified pipeline from being graded.
		return nil, fmt.Errorf("merge request %s reported no source commit", pr.Number)
	}
	if !sameCommitSHA(revision, headSHA) {
		return nil, fmt.Errorf("%w: merge request %s source commit is %s, not the commit being delivered %s", scm.ErrHeadChanged, pr.Number, revision, headSHA)
	}
	pipeline, err := mr.headPipeline()
	if err != nil {
		return nil, fmt.Errorf("merge request %s: %w", pr.Number, err)
	}
	if pipeline == nil {
		return nil, nil
	}
	if strings.TrimSpace(pipeline.SHA) == "" {
		return nil, fmt.Errorf("merge request %s head pipeline %d reported no commit", pr.Number, pipeline.ID)
	}
	if sameCommitSHA(pipeline.SHA, headSHA) {
		return pipeline, nil
	}
	// A pipeline that ran at another commit is only evidence for headSHA when it
	// is the merge request's merged-results pipeline and its temporary commit
	// includes headSHA. Anything else - a branch pipeline from before the last
	// push, most of all - cannot be attached to this head.
	if !mergeResultRef(pipeline.Ref, pr.Number) {
		return nil, fmt.Errorf("%w: merge request %s head pipeline %d ran at %s (%s), not at the commit being delivered %s", scm.ErrHeadChanged, pr.Number, pipeline.ID, pipeline.SHA, strings.TrimSpace(pipeline.Ref), headSHA)
	}
	includes, err := h.mergeResultIncludes(ctx, pipeline.SHA, headSHA)
	if err != nil {
		return nil, err
	}
	if !includes {
		return nil, fmt.Errorf("%w: merge request %s merged-results pipeline %d ran at %s, which does not include the commit being delivered %s", scm.ErrHeadChanged, pr.Number, pipeline.ID, pipeline.SHA, headSHA)
	}
	return pipeline, nil
}

// mergeResultRef reports whether ref names one of this merge request's
// merged-results refs. GitLab builds both refs from a temporary commit that
// merges the merge request's source revision into the target side, so a
// pipeline running there carries a commit the source branch does not have:
// refs/merge-requests/<iid>/merge for merged results and
// refs/merge-requests/<iid>/train for merge trains.
func mergeResultRef(ref, number string) bool {
	number = strings.TrimSpace(number)
	if number == "" {
		return false
	}
	trimmed := strings.TrimSpace(ref)
	for _, suffix := range []string{"/merge", "/train"} {
		if trimmed == "refs/merge-requests/"+number+suffix {
			return true
		}
	}
	return false
}

// mergeResultIncludes proves that the temporary commit a merged-results
// pipeline ran at contains revision. GitLab creates that commit by merging the
// merge request's source revision into the target side
// (MergeRequests::MergeToRefService), so the source commit is one of the merge
// commit's parents; parentage is the provenance, and it is deliberately checked
// against the repository rather than assumed from the SHA difference, so a
// stale merged-results pipeline from before the latest push fails it.
func (h *Host) mergeResultIncludes(ctx context.Context, mergedSHA, revision string) (bool, error) {
	if h.projectPath == "" {
		return false, errors.New("cannot verify a merged-results pipeline includes the delivered commit: GitLab project path unknown")
	}
	mergedSHA = strings.TrimSpace(mergedSHA)
	revision = strings.TrimSpace(revision)
	if !isHexCommitSHA(mergedSHA) {
		return false, fmt.Errorf("read GitLab merge-result commit %s: not a commit id", mergedSHA)
	}
	cmd := h.cmd(ctx, "glab", "api", fmt.Sprintf("projects/%s/repository/commits/%s", encodeProjectPath(h.projectPath), mergedSHA))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("read GitLab merge-result commit %s: %s: %w", mergedSHA, strings.TrimSpace(string(out)), err)
	}
	var commit struct {
		ID        string   `json:"id"`
		ParentIDs []string `json:"parent_ids"`
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 || json.Unmarshal(trimmed, &commit) != nil {
		return false, fmt.Errorf("read GitLab merge-result commit %s: invalid JSON output: %s", mergedSHA, strings.TrimSpace(string(out)))
	}
	if !sameCommitSHA(commit.ID, mergedSHA) {
		return false, fmt.Errorf("read GitLab merge-result commit %s: response identified commit %s", mergedSHA, strings.TrimSpace(commit.ID))
	}
	for _, parent := range commit.ParentIDs {
		if sameCommitSHA(parent, revision) {
			return true, nil
		}
	}
	return false, nil
}

// sameCommitSHA compares two commit identities, tolerating the surrounding
// whitespace and letter case a provider response may carry.
func sameCommitSHA(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// isHexCommitSHA reports whether value is a full hexadecimal commit id: the
// only shape allowed into a repository API path.
func isHexCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// readPipelineJobs lists every job of a pipeline. With a known project path it
// goes through `glab api --paginate` (branch-independent); otherwise it falls
// back to `glab ci get`, which needs a current branch.
//
// The JSON comes from stdout alone: glab prints notices (an update notice, for
// example) on stderr, and merging the streams would splice that text into the
// listing the strict parser reads.
func (h *Host) readPipelineJobs(ctx context.Context, pipelineID int) ([]byte, error) {
	jobsCmd := h.cmd(ctx, "glab", h.pipelineJobsArgs(pipelineID)...)
	var stderr bytes.Buffer
	jobsCmd.Stderr = &stderr
	jobsOut, err := jobsCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("glab pipeline jobs: %s: %w", strings.TrimSpace(string(append(jobsOut, stderr.Bytes()...))), err)
	}
	return jobsOut, nil
}

// pipelineTriggerJobs lists a pipeline's trigger jobs. GitLab 19.2 renamed the
// route from "bridges" to "trigger_jobs" and deprecated the old name; neither
// is removed. The current name is read first, and an instance that predates it
// answers with GitLab's route-not-found response, so the deprecated name is
// retried once for that instance rather than failing a read it can still serve.
// glabStdout runs glab and returns stdout alone for the JSON decoder, plus
// stdout and stderr together for failure classification and messages: glab
// prints notices on stderr, which must not reach a successful read's decoder.
func (h *Host) glabStdout(ctx context.Context, args []string) (stdout, combined []byte, err error) {
	cmd := h.cmd(ctx, "glab", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err = cmd.Output()
	return stdout, append(append([]byte{}, stdout...), stderr.Bytes()...), err
}

func (h *Host) pipelineTriggerJobs(ctx context.Context, pipelineID int) ([]gitlabTriggerJob, error) {
	out, combined, err := h.glabStdout(ctx, h.pipelineRouteArgs(pipelineID, "trigger_jobs"))
	if err != nil && gitlabRouteUnavailable(combined) {
		out, combined, err = h.glabStdout(ctx, h.pipelineRouteArgs(pipelineID, "bridges"))
	}
	if err != nil {
		return nil, fmt.Errorf("glab pipeline trigger jobs: %s: %w", strings.TrimSpace(string(combined)), err)
	}
	triggerJobs, decodeErr := decodeGitlabTriggerJobs(out)
	if decodeErr != nil {
		return nil, decodeErr
	}
	return triggerJobs, nil
}

// mergeRequestStatusChecks lists the external status check services that apply
// to the merge request and their current state. External status checks are a
// GitLab Ultimate capability: Community Edition does not mount the route at
// all, and an Enterprise Edition instance without the license answers 401 from
// the route's license guard. Those two mean "this instance has no external
// status checks to report" and read as none - treating them as unreadable
// would park every harness run on an instance where the feature simply was not
// sold. Every other failure is a read failure, including 403: the license
// guard answers 401 before a 403 is possible, so a 403 is a project that does
// have the feature refusing this token, and reading it as "no checks" would
// hide one.
func (h *Host) mergeRequestStatusChecks(ctx context.Context, mrIID string) ([]scm.Check, error) {
	out, combined, err := h.glabStdout(ctx, h.mergeRequestStatusChecksArgs(mrIID))
	if err != nil {
		if gitlabExternalStatusChecksUnavailable(combined) {
			return nil, nil
		}
		return nil, fmt.Errorf("glab merge request status checks: %s: %w", strings.TrimSpace(string(combined)), err)
	}
	checks, decodeErr := decodeGitlabExternalStatusChecks(out)
	if decodeErr != nil {
		return nil, decodeErr
	}
	return externalStatusChecksToChecks(checks), nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	targets := make([]scm.CheckTarget, 0, len(failingNames))
	for _, name := range failingNames {
		targets = append(targets, scm.CheckTarget{Name: name})
	}
	logs, err := h.FetchFailedCheckTargetLogs(ctx, pr, branch, headSHA, targets)
	if err != nil {
		return "", err
	}
	return scm.CombineFailedCheckLogs(logs)
}

// FetchFailedCheckTargetLogs traces the selected failures on the same pipeline
// identity the check observation was bound to. When the caller names the
// delivered commit, the pipeline is proven to belong to it (see
// headPipelineBoundTo) before any trace is read, so a selected check can never
// be explained by a job of another commit's pipeline: GitLab's head pipeline
// pointer lags a new source commit, and a same-named job in a newer or older
// pipeline is not the check that failed. The merge request is then re-read, and
// a source revision that moved while the traces were being fetched is rejected
// exactly as in GetChecks. Without a named commit there is nothing to bind to,
// and the legacy head-pipeline read is kept.
func (h *Host) FetchFailedCheckTargetLogs(ctx context.Context, pr *scm.PR, _ string, headSHA string, targets []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	// Get the MR's pipeline jobs and trace the selected failures.
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("resolve GitLab merge request for selected logs: %w", err)
	}
	bound := false
	var pipeline *gitlabHeadPipeline
	if headSHA = strings.TrimSpace(headSHA); headSHA != "" {
		bound = true
		pipeline, err = h.headPipelineBoundTo(ctx, pr, mr, headSHA)
		if err != nil {
			return nil, fmt.Errorf("resolve GitLab pipeline for selected logs: %w", err)
		}
	} else {
		pipeline, err = mr.headPipeline()
		if err != nil {
			return nil, fmt.Errorf("resolve GitLab pipeline for selected logs: %w", err)
		}
	}
	if pipeline == nil {
		return nil, errors.New("resolve GitLab pipeline for selected logs: pipeline ID is empty")
	}
	jobsOut, err := h.readPipelineJobs(ctx, pipeline.ID)
	if err != nil {
		return nil, fmt.Errorf("list GitLab jobs for selected logs: %w", err)
	}
	results := make([]scm.FailedCheckLog, 0, len(targets))
	for _, target := range targets {
		result := scm.FailedCheckLog{Target: target}
		jobIDs := findFailedJobTargetIDs(jobsOut, []scm.CheckTarget{target})
		if len(jobIDs) == 0 {
			result.Err = fmt.Errorf("selected GitLab check %q was not found", target.Identity())
			results = append(results, result)
			continue
		}
		var outputs []string
		var errs []error
		for _, jobID := range jobIDs {
			traceCmd := h.cmd(ctx, "glab", "ci", "trace", fmt.Sprintf("%d", jobID))
			traceOut, err := traceCmd.Output()
			if err != nil {
				errs = append(errs, fmt.Errorf("fetch GitLab job %d trace: %w", jobID, err))
				continue
			}
			if log := strings.TrimSpace(string(traceOut)); log != "" {
				outputs = append(outputs, log)
			}
		}
		result.Output = strings.Join(outputs, "\n\n")
		result.Err = errors.Join(errs...)
		results = append(results, result)
	}
	if bound {
		after, err := h.viewMR(ctx, pr.Number)
		if err != nil {
			return nil, fmt.Errorf("re-read GitLab merge request after selected logs: %w", err)
		}
		if revision := after.sourceRevision(); revision == "" {
			return nil, fmt.Errorf("merge request %s reported no source commit", pr.Number)
		} else if !sameCommitSHA(revision, headSHA) {
			return nil, fmt.Errorf("%w: merge request %s source commit changed during log retrieval from %s to %s", scm.ErrHeadChanged, pr.Number, headSHA, revision)
		}
	}
	return results, nil
}

func parseMRPayload(out []byte) (mrPayload, bool) {
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return mrPayload{}, false
	}
	var mr mrPayload
	if err := json.Unmarshal(trimmed, &mr); err != nil {
		return mrPayload{}, false
	}
	return mr, true
}

func bytesTrimToJSON(out []byte) []byte {
	// glab may emit a banner line before JSON; skip until '{'.
	idx := -1
	for i, b := range out {
		if b == '{' || b == '[' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	return out[idx:]
}

type gitlabJob struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	FinishedAt string `json:"finished_at"`
	WebURL     string `json:"web_url"`
	// AllowFailure marks a job GitLab lets fail without failing the pipeline
	// (`allow_failure: true`). Such a job reports status "failed" but does not
	// block the merge request.
	AllowFailure bool `json:"allow_failure"`
}

// bucket maps the job to a check bucket. A failed job with allow_failure set
// is non-blocking in GitLab (the pipeline is green, shown as "passed with
// warnings"), so it maps to CheckBucketSkip like other non-blocking outcomes
// instead of CheckBucketFail.
//
// A manual job's bucket depends on the same flag, and the two readings are
// opposites. A blocking manual job (`allow_failure: false`, the default for
// `when: manual` inside `rules`) stops the pipeline at its stage until a person
// runs it, and GitLab's own merge rules treat that pipeline as
// unfinished; mapping it to a skip lets a merge request read as ready while
// GitLab would refuse to merge it. It maps to CheckBucketPending instead - the
// job is waiting on a person, not finished - and jobsToChecks marks it
// AwaitingManualJob so the CI step names the wait and keeps it away from the
// fix agent. An optional manual job (`allow_failure: true`, the default outside
// `rules`) is one GitLab does not wait for - the pipeline can succeed without
// it - so it stays a skip.
func (j gitlabJob) bucket() scm.CheckBucket {
	status := strings.ToLower(strings.TrimSpace(j.Status))
	switch {
	case status == "manual":
		if j.AllowFailure {
			return scm.CheckBucketSkip
		}
		return scm.CheckBucketPending
	case j.AllowFailure && status == "failed":
		return scm.CheckBucketSkip
	}
	return gitlabStatusBucket(j.Status)
}

// awaitingManualJob reports whether the job is a blocking manual job: GitLab
// holds the pipeline at it until a person runs it. It is the one
// pending check that cannot progress on its own and that no rerun can clear.
func (j gitlabJob) awaitingManualJob() bool {
	return strings.EqualFold(strings.TrimSpace(j.Status), "manual") && !j.AllowFailure
}

// completedAt parses the job's finished_at timestamp, returning the zero time
// when it is absent or unparseable. GitLab emits RFC3339 (often with
// fractional seconds and a 'Z' offset), which time.RFC3339 handles.
func (j gitlabJob) completedAt() time.Time {
	if strings.TrimSpace(j.FinishedAt) == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, j.FinishedAt); err == nil {
		return parsed
	}
	return time.Time{}
}

// gitlabTriggerJob is a pipeline trigger job (historically "bridge"): the job
// that starts a downstream pipeline. It carries an ordinary job's fields plus
// the downstream pipeline it created, whose result GitLab mirrors into the
// trigger job's own status when the trigger asks for it (trigger:strategy).
type gitlabTriggerJob struct {
	gitlabJob
	DownstreamPipeline *gitlabDownstreamPipeline `json:"downstream_pipeline"`
}

type gitlabDownstreamPipeline struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

// gitlabExternalStatusCheck is one external status check service's verdict on
// a merge request (GitLab Ultimate).
type gitlabExternalStatusCheck struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ExternalURL string `json:"external_url"`
	Status      string `json:"status"`
}

// decodeGitlabJobs reads every job from glab output. The output may contain a
// single bare job array, a pipeline object with nested .jobs, or - when
// `glab api --paginate` walks multiple pages - several JSON documents
// concatenated back to back (one array per page). A streaming decoder reads
// each top-level value in turn and accumulates the jobs across all of them.
//
// Every page must read in full. Output with no JSON, a document that is
// neither a job array nor a pipeline object with a "jobs" field, and a job
// entry without a name or status each return an error. GitLab always sends
// both fields, so a document without them is not a job listing. Skipping such
// a page would hide any failed job on it, and an unreadable response would
// read as an empty, green pipeline. io.EOF ends the stream cleanly; when a
// later page is malformed or the wrong shape, the jobs from earlier pages are
// returned along with the error.
func decodeGitlabJobs(out []byte) ([]gitlabJob, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("decode gitlab jobs: response contained no JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var jobs []gitlabJob
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return jobs, nil
		}
		if err != nil {
			// Malformed mid-stream document: stop, but keep what parsed so far
			// and report the error rather than silently swallowing the page.
			return jobs, fmt.Errorf("decode gitlab jobs: %w", err)
		}
		page, err := decodeGitlabJobPage(raw)
		if err != nil {
			return jobs, err
		}
		jobs = append(jobs, page...)
	}
}

// decodeGitlabJobPage reads one top-level JSON document from glab output:
// either a bare array of jobs (`glab api --paginate`) or a pipeline object
// with a "jobs" field containing an array or null for a pipeline with no jobs
// (`glab ci get --with-job-details`). A top-level null is invalid.
// Every entry must be a job object with a non-empty name and status, or the
// read fails.
func decodeGitlabJobPage(raw json.RawMessage) ([]gitlabJob, error) {
	trimmed := bytes.TrimSpace(raw)
	var entries []json.RawMessage
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("decode gitlab jobs: %w", err)
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		var page struct {
			Jobs json.RawMessage `json:"jobs"`
		}
		if err := json.Unmarshal(trimmed, &page); err != nil {
			return nil, fmt.Errorf("decode gitlab jobs: %w", err)
		}
		pageJobs := bytes.TrimSpace(page.Jobs)
		if len(pageJobs) == 0 {
			return nil, errors.New(`decode gitlab jobs: pipeline object has no "jobs" field`)
		}
		// glab builds this object's job list with client-go's ScanAndCollect,
		// which returns a nil slice for a pipeline with no jobs, and its
		// PrintJSON turns only a top-level nil slice into []. A pipeline with
		// no jobs therefore prints a present "jobs": null here, the same empty
		// listing the REST route reports as []. Only a missing key is
		// unreadable.
		if bytes.Equal(pageJobs, []byte("null")) {
			return nil, nil
		}
		if err := json.Unmarshal(pageJobs, &entries); err != nil {
			return nil, fmt.Errorf(`decode gitlab jobs: "jobs" is not an array: %w`, err)
		}
	default:
		return nil, fmt.Errorf("decode gitlab jobs: page is neither a job array nor a pipeline object: %.80s", trimmed)
	}
	jobs := make([]gitlabJob, 0, len(entries))
	for _, entry := range entries {
		var job gitlabJob
		if err := json.Unmarshal(entry, &job); err != nil {
			return nil, fmt.Errorf("decode gitlab jobs: %w", err)
		}
		if strings.TrimSpace(job.Name) == "" {
			return nil, errors.New("decode gitlab jobs: job entry has no name")
		}
		if strings.TrimSpace(job.Status) == "" {
			return nil, fmt.Errorf("decode gitlab jobs: job %q has no status", job.Name)
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// decodeGitlabPages streams the top-level JSON documents of glab output, one
// per paginated page, and hands each to decodePage. Output with no JSON at all
// is unreadable rather than an empty listing: GitLab answers a route it serves
// with at least one document, so reading silence as "nothing to report" would
// hide a failed check. io.EOF ends the stream cleanly; a document that does not
// decode returns the values collected so far with the error.
func decodeGitlabPages(what string, out []byte, decodePage func(json.RawMessage) error) error {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return fmt.Errorf("decode gitlab %s: response contained no JSON", what)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode gitlab %s: %w", what, err)
		}
		if err := decodePage(raw); err != nil {
			return err
		}
	}
}

// decodeGitlabTriggerJobs reads every trigger job from the paginated REST
// route, which emits one bare array per page. An entry without a name or status
// is not a trigger job and fails the read, for the same reason the job decoder
// refuses one: silently skipping it would hide the row that reports a failed
// downstream pipeline.
func decodeGitlabTriggerJobs(out []byte) ([]gitlabTriggerJob, error) {
	var jobs []gitlabTriggerJob
	err := decodeGitlabPages("trigger jobs", out, func(raw json.RawMessage) error {
		var page []gitlabTriggerJob
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			return fmt.Errorf("decode gitlab trigger jobs: page is not an array")
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode gitlab trigger jobs: %w", err)
		}
		for _, job := range page {
			if strings.TrimSpace(job.Name) == "" {
				return errors.New("decode gitlab trigger jobs: entry has no name")
			}
			if strings.TrimSpace(job.Status) == "" {
				return fmt.Errorf("decode gitlab trigger jobs: %q has no status", job.Name)
			}
		}
		jobs = append(jobs, page...)
		return nil
	})
	return jobs, err
}

// decodeGitlabExternalStatusChecks reads every external status check from the
// paginated REST route, which emits one bare array per page. The same
// strictness applies: an entry without a name or status is not a status check.
func decodeGitlabExternalStatusChecks(out []byte) ([]gitlabExternalStatusCheck, error) {
	var checks []gitlabExternalStatusCheck
	err := decodeGitlabPages("external status checks", out, func(raw json.RawMessage) error {
		var page []gitlabExternalStatusCheck
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			return fmt.Errorf("decode gitlab external status checks: page is not an array")
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode gitlab external status checks: %w", err)
		}
		for _, check := range page {
			if strings.TrimSpace(check.Name) == "" {
				return errors.New("decode gitlab external status checks: entry has no name")
			}
			if strings.TrimSpace(check.Status) == "" {
				return fmt.Errorf("decode gitlab external status checks: %q has no status", check.Name)
			}
		}
		checks = append(checks, page...)
		return nil
	})
	return checks, err
}

func parseGitlabJobs(out []byte) ([]scm.Check, error) {
	jobs, err := decodeGitlabJobs(out)
	if len(jobs) == 0 {
		return nil, err
	}
	// Surface any decode error even when some jobs parsed. A corrupt later page
	// of paginated `glab api` output must not let a partial slice look
	// authoritative: a failed job on the dropped page would otherwise be hidden
	// and the CI verdict would read green.
	return jobsToChecks(jobs), err
}

func jobsToChecks(jobs []gitlabJob) []scm.Check {
	checks := make([]scm.Check, 0, len(jobs))
	for _, job := range jobs {
		checks = append(checks, scm.Check{
			Name:              job.Name,
			ProviderID:        gitlabJobProviderID("gitlab-job", job.ID),
			Bucket:            job.bucket(),
			Kind:              scm.CheckKindRun,
			State:             gitlabCheckState(job.Status),
			CompletedAt:       job.completedAt(),
			Link:              strings.TrimSpace(job.WebURL),
			AwaitingManualJob: job.awaitingManualJob(),
		})
	}
	return checks
}

// triggerJobsToChecks maps trigger jobs to checks. The status is the trigger
// job's own - GitLab computes it from whether the downstream pipeline was
// created, or, when the trigger asks for it, from the downstream pipeline's
// result - so it is exactly the part of the downstream tree that the head
// pipeline's aggregate reflects. The details link prefers the downstream
// pipeline because that is where a failure has to be read.
func triggerJobsToChecks(jobs []gitlabTriggerJob) []scm.Check {
	checks := make([]scm.Check, 0, len(jobs))
	for _, job := range jobs {
		link := strings.TrimSpace(job.WebURL)
		if job.DownstreamPipeline != nil {
			if url := strings.TrimSpace(job.DownstreamPipeline.WebURL); url != "" {
				link = url
			}
		}
		checks = append(checks, scm.Check{
			Name:              job.Name,
			ProviderID:        gitlabJobProviderID("gitlab-trigger-job", job.ID),
			Bucket:            job.bucket(),
			Kind:              scm.CheckKindRun,
			State:             gitlabCheckState(job.Status),
			CompletedAt:       job.completedAt(),
			Link:              link,
			AwaitingManualJob: job.awaitingManualJob(),
		})
	}
	return checks
}

// externalStatusChecksToChecks maps external status checks to checks. GitLab
// reports one of passed, pending, or failed for each service that applies to
// the merge request; a state this version does not recognize stays pending so
// readiness waits instead of certifying a state it cannot classify.
func externalStatusChecksToChecks(statusChecks []gitlabExternalStatusCheck) []scm.Check {
	checks := make([]scm.Check, 0, len(statusChecks))
	for _, status := range statusChecks {
		id := ""
		if status.ID != 0 {
			id = "gitlab-status-check:" + strconv.Itoa(status.ID)
		}
		checks = append(checks, scm.Check{
			Name:       status.Name,
			ProviderID: id,
			Bucket:     gitlabExternalStatusCheckBucket(status.Status),
			Kind:       scm.CheckKindStatus,
			State:      gitlabCheckState(status.Status),
			Link:       strings.TrimSpace(status.ExternalURL),
		})
	}
	return checks
}

// pipelineAggregateCheck returns the head pipeline's own status as a check when
// no job or trigger job row already carries it, and no check when one does. The aggregate
// is what the merge gate reads, so it must never be hidden by a job list that
// happens to look green - a failed pipeline that created no job at all, or one
// still running after its last job finished, has no row in the job list to
// carry the state. A terminal pass or skip needs no row of its own: every row
// that reports one already says the pipeline is not blocking.
func pipelineAggregateCheck(pipeline gitlabHeadPipeline, checks []scm.Check) (scm.Check, bool) {
	bucket := gitlabStatusBucket(pipeline.Status)
	if bucket == "" {
		// An unrecognized or incomplete pipeline state must not certify the
		// commit as green; keep monitoring until GitLab reports a state that can
		// be classified.
		bucket = scm.CheckBucketPending
	}
	switch bucket {
	case scm.CheckBucketFail, scm.CheckBucketCancel, scm.CheckBucketPending:
	default:
		return scm.Check{}, false
	}
	for _, check := range checks {
		if check.Bucket == bucket {
			return scm.Check{}, false
		}
	}
	return scm.Check{
		Name:              fmt.Sprintf("Pipeline #%d", pipeline.ID),
		ProviderID:        "gitlab-pipeline:" + strconv.Itoa(pipeline.ID),
		Bucket:            bucket,
		Kind:              scm.CheckKindRun,
		State:             gitlabCheckState(pipeline.Status),
		CompletedAt:       pipeline.completedAt(),
		Link:              strings.TrimSpace(pipeline.WebURL),
		AwaitingManualJob: pipeline.awaitingManualJob(),
	}, true
}

// gitlabJobProviderID renders the provider identity of a job or trigger job,
// empty when GitLab reported no id (an unidentified row must not collide with
// another unnamed row in the fix-target bookkeeping).
func gitlabJobProviderID(prefix string, id int) string {
	if id == 0 {
		return ""
	}
	return prefix + ":" + strconv.Itoa(id)
}

// gitlabCheckState renders the provider's own status string the way the other
// adapters do: uppercased and otherwise untouched, so a finding can say what
// the provider reported without this package inventing vocabulary.
func gitlabCheckState(state string) string {
	return strings.ToUpper(strings.TrimSpace(state))
}

func findFailedJobTargetIDs(out []byte, checkTargets []scm.CheckTarget) []int {
	names := map[string]struct{}{}
	ids := map[string]struct{}{}
	for _, target := range checkTargets {
		if id := strings.TrimSpace(target.ProviderID); id != "" {
			ids[id] = struct{}{}
		} else if name := strings.TrimSpace(target.Name); name != "" {
			names[name] = struct{}{}
		}
	}
	// Best effort: scan whatever jobs parsed; a corrupt later page does not
	// prevent locating a failed job that already decoded.
	jobs, _ := decodeGitlabJobs(out)
	var matched []int
	for _, job := range jobs {
		if !strings.EqualFold(job.Status, "failed") || job.AllowFailure {
			continue
		}
		_, nameMatch := names[job.Name]
		_, idMatch := ids[fmt.Sprintf("gitlab-job:%d", job.ID)]
		if nameMatch || idMatch || len(names)+len(ids) == 0 {
			matched = append(matched, job.ID)
		}
	}
	return matched
}

// gitlabStatusBucket maps a raw provider state - a job's or a pipeline's -
// onto the shared bucket, ignoring job-level policy; gitlabJob.bucket applies
// allow_failure on top for the jobs it maps. `manual` is a wait on a person,
// not a finished state: a pipeline's own status is `manual` while it is
// stopped at a blocking manual job, and reporting that as a skip would let a
// blocked pipeline read green. Job-level manual states never reach this case -
// gitlabJob.bucket splits them by allow_failure first - so this mapping serves
// the pipeline-status reading, not the job reader.
func gitlabStatusBucket(state string) scm.CheckBucket {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success":
		return scm.CheckBucketPass
	case "failed":
		return scm.CheckBucketFail
	case "canceled", "cancelled":
		return scm.CheckBucketCancel
	case "skipped":
		return scm.CheckBucketSkip
	case "manual", "pending", "running", "created", "waiting_for_resource", "preparing", "scheduled":
		return scm.CheckBucketPending
	default:
		return ""
	}
}

// gitlabExternalStatusCheckBucket maps an external status check's state. GitLab
// reports passed, pending, or failed; anything else is a state this version
// cannot classify, and waiting is the safe reading of an unknown state (a
// failed external status check blocks merges, so it must never be missed, and
// an unrecognized one must not be optimistically read as passed). Unlike a job,
// it has no skipped or cancelled state to map.
func gitlabExternalStatusCheckBucket(state string) scm.CheckBucket {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "passed":
		return scm.CheckBucketPass
	case "failed":
		return scm.CheckBucketFail
	default:
		return scm.CheckBucketPending
	}
}

// gitlabRouteUnavailable reports whether a failed glab call means the instance
// does not serve the requested REST route at all, rather than answering it with
// an error: GitLab responses to an unmounted route are a bare
// `{"error":"404 Not Found"}`, which glab prints as `glab: HTTP 404`.
// Distinguishing the two matters for routes that were added or renamed between
// GitLab versions - a missing route is a version difference to work around,
// while an answered error is a failed read. A resource-level 404 ("Project Not
// Found") does not match: glab prints `glab: 404 Project Not Found (HTTP 404)`
// for it, which is neither pattern, so it fails the read instead of being
// mistaken for a version difference.
func gitlabRouteUnavailable(out []byte) bool {
	text := string(out)
	return strings.Contains(text, `"error":"404 Not Found"`) || strings.Contains(text, "glab: HTTP 404")
}

// gitlabExternalStatusChecksUnavailable reports whether a failed status-checks
// read means the instance does not serve external status checks rather than the
// read failing. GitLab mounts the route only on Enterprise Edition with an
// Ultimate license: Community Edition answers 404 with the unmounted-route body
// (live-observed), and an unlicensed Enterprise Edition instance answers 401
// from the route's own license guard (`unauthorized!`). A 403 is deliberately
// not in this set: the license guard answers 401 first, so a 403 comes from a
// project that does have the feature and refused this token (or from a
// granular token without the read permission) - a failed read, not an absent
// feature. A network error, 5xx, or malformed answer matches none of these and
// must surface as well.
func gitlabExternalStatusChecksUnavailable(out []byte) bool {
	text := string(out)
	if strings.Contains(text, `"error":"404 Not Found"`) {
		return true
	}
	for _, refusal := range []string{"HTTP 401", `"message":"401 Unauthorized"`} {
		if strings.Contains(text, refusal) {
			return true
		}
	}
	return false
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "opened", "open":
		return scm.PRStateOpen
	case "merged":
		return scm.PRStateMerged
	case "closed", "locked":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(raw))
	}
}

func extractMRURL(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
	}
	trimmed := bytesTrimToJSON(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"web_url", "url", "webUrl"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

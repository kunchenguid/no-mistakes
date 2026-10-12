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
// pipeline ran at and the ref it ran for, next to its identifier.
type gitlabHeadPipeline struct {
	ID  int    `json:"id"`
	SHA string `json:"sha"`
	Ref string `json:"ref"`
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

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	if headSHA := strings.TrimSpace(pr.HeadSHA); headSHA != "" {
		return h.getChecksForHead(ctx, pr, headSHA)
	}
	// Without a named source commit there is nothing to bind a read to, so the
	// legacy paths stay: glab ci status --mr <id> --output json lists jobs for
	// the MR's latest pipeline, and versions that do not support --mr fall back
	// to listing the head pipeline's jobs through mr view.
	cmd := h.cmd(ctx, "glab", "ci", "status", "--mr", pr.Number, "--output", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		diagnostic := append(out, stderr.Bytes()...)
		if !isUnsupportedMRFlagError(diagnostic) {
			return nil, fmt.Errorf("glab ci status: %s: %w", strings.TrimSpace(string(diagnostic)), err)
		}
		return h.getChecksFallback(ctx, pr)
	}
	return parseGitlabJobs(out)
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
// mergeResultIncludes). It then re-reads the merge request after the job read
// and rejects a source revision that moved mid-observation, exactly as the
// GitHub adapter does.
func (h *Host) getChecksForHead(ctx context.Context, pr *scm.PR, headSHA string) ([]scm.Check, error) {
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	pipeline, err := h.headPipelineBoundTo(ctx, pr, mr, headSHA)
	if err != nil {
		return nil, err
	}
	if pipeline == nil {
		// No head pipeline at all: no check has registered for this commit yet,
		// so the caller gets an empty observation and keeps waiting rather than
		// a verdict it cannot support.
		return nil, nil
	}
	jobsOut, err := h.readPipelineJobs(ctx, pipeline.ID)
	if err != nil {
		return nil, err
	}
	checks, err := parseGitlabJobs(jobsOut)
	if err != nil {
		return nil, err
	}
	after, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	if revision := after.sourceRevision(); revision == "" {
		return nil, fmt.Errorf("merge request %s reported no source commit", pr.Number)
	} else if !sameCommitSHA(revision, headSHA) {
		return nil, fmt.Errorf("%w: merge request %s source commit changed during check discovery from %s to %s", scm.ErrHeadChanged, pr.Number, headSHA, revision)
	}
	return checks, nil
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

func isUnsupportedMRFlagError(out []byte) bool {
	msg := strings.ToLower(strings.TrimSpace(string(out)))
	if !strings.Contains(msg, "--mr") {
		return false
	}
	for _, marker := range []string{
		"unknown flag",
		"unknown option",
		"unsupported flag",
		"unsupported option",
		"unrecognized argument",
		"unrecognized arguments",
		"unrecognized option",
		"unknown argument",
		"unexpected argument",
		"flag provided but not defined",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func (h *Host) getChecksFallback(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	// Try fetching the MR's pipeline and listing its jobs.
	mr, err := h.viewMR(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	pipeline, err := mr.headPipeline()
	if err != nil {
		return nil, fmt.Errorf("glab mr view: %w", err)
	}
	if pipeline == nil {
		// The MR reports "head_pipeline": null, meaning it has no pipeline.
		// That is the only shape that reads as no checks; headPipeline
		// rejects the rest.
		return nil, nil
	}
	jobsOut, err := h.readPipelineJobs(ctx, pipeline.ID)
	if err != nil {
		return nil, err
	}
	return parseGitlabJobs(jobsOut)
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
	// AllowFailure marks a job GitLab lets fail without failing the pipeline
	// (`allow_failure: true`). Such a job reports status "failed" but does not
	// block the merge request.
	AllowFailure bool `json:"allow_failure"`
}

// bucket maps the job to a check bucket. A failed job with allow_failure set
// is non-blocking in GitLab (the pipeline is green, shown as "passed with
// warnings"), so it maps to CheckBucketSkip like other non-blocking outcomes
// instead of CheckBucketFail.
func (j gitlabJob) bucket() scm.CheckBucket {
	if j.AllowFailure && strings.EqualFold(strings.TrimSpace(j.Status), "failed") {
		return scm.CheckBucketSkip
	}
	return gitlabStatusBucket(j.Status)
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
		providerID := ""
		if job.ID != 0 {
			providerID = fmt.Sprintf("gitlab-job:%d", job.ID)
		}
		checks = append(checks, scm.Check{
			Name:        job.Name,
			ProviderID:  providerID,
			Bucket:      job.bucket(),
			CompletedAt: job.completedAt(),
		})
	}
	return checks
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
	case "manual":
		return scm.CheckBucketSkip
	case "pending", "running", "created", "waiting_for_resource", "preparing", "scheduled":
		return scm.CheckBucketPending
	default:
		return ""
	}
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

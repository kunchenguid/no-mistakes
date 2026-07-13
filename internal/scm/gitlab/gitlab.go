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
	"strings"
	"time"

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
		// GitLab's REST API wants the project as a single URL-encoded
		// "group%2Fproject" path parameter. Escape each segment defensively and
		// rejoin with %2F so any reserved character in a segment is encoded too,
		// not just the separating slashes.
		segments := strings.Split(h.projectPath, "/")
		for i, seg := range segments {
			segments[i] = url.PathEscape(seg)
		}
		enc := strings.Join(segments, "%2F")
		// --paginate walks every page; a pipeline with more jobs than fit on one
		// page (GitLab defaults to 20 per page) would otherwise silently drop the
		// jobs on later pages and the CI verdict could miss a failed job. glab
		// writes one JSON array per page, so the parser handles concatenated docs.
		return []string{"api", "--paginate", fmt.Sprintf("projects/%s/pipelines/%d/jobs", enc, pipelineID)}
	}
	return []string{"ci", "get", "--pipeline-id", fmt.Sprintf("%d", pipelineID), "--output", "json", "--with-job-details"}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitLab }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true}
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
	if err := scm.RunAuthProbe(func() *exec.Cmd { return h.cmd(ctx, "glab", authArgs...) }); err != nil {
		return errors.New("glab CLI is not authenticated")
	}
	return nil
}

type mrPayload struct {
	IID                 int    `json:"iid"`
	WebURL              string `json:"web_url"`
	URL                 string `json:"url"`
	State               string `json:"state"`
	HasConflicts        bool   `json:"has_conflicts"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	MergeStatus         string `json:"merge_status"`
}

func (p mrPayload) toPR() *scm.PR {
	url := strings.TrimSpace(p.WebURL)
	if url == "" {
		url = strings.TrimSpace(p.URL)
	}
	pr := &scm.PR{URL: url}
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
		return nil, nil
	}
	var mrs []mrPayload
	if err := json.Unmarshal(trimmed, &mrs); err != nil || len(mrs) == 0 {
		return nil, nil
	}
	pr := mrs[0].toPR()
	if pr.URL == "" {
		return nil, nil
	}
	return pr, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	cmd := h.cmd(ctx, "glab", "mr", "create",
		"--source-branch", branch,
		"--target-branch", base,
		"--title", content.Title,
		"--description", content.Body,
		"--yes",
	)
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
	cmd := h.cmd(ctx, "glab", "mr", "update", id,
		"--title", content.Title,
		"--description", content.Body,
		"--yes",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("glab mr update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
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
	// glab ci status --mr <id> --output json lists jobs for the MR's latest pipeline.
	// Not all glab versions support --mr; fall back to listing pipelines by branch via view.
	cmd := h.cmd(ctx, "glab", "ci", "status", "--mr", pr.Number, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if !isUnsupportedMRFlagError(out) {
			return nil, fmt.Errorf("glab ci status: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return h.getChecksFallback(ctx, pr)
	}
	return parseGitlabJobs(out)
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
	cmd := h.cmd(ctx, "glab", "mr", "view", pr.Number, "--output", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab mr view: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload struct {
		HeadPipeline struct {
			ID int `json:"id"`
		} `json:"head_pipeline"`
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("glab mr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, fmt.Errorf("glab mr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if payload.HeadPipeline.ID == 0 {
		return nil, nil
	}
	jobsCmd := h.cmd(ctx, "glab", h.pipelineJobsArgs(payload.HeadPipeline.ID)...)
	jobsOut, err := jobsCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab pipeline jobs: %s: %w", strings.TrimSpace(string(jobsOut)), err)
	}
	return parseGitlabJobs(jobsOut)
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, _ string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	// Get the MR's pipeline jobs, find a failed one whose name matches, trace it.
	viewCmd := h.cmd(ctx, "glab", "mr", "view", pr.Number, "--output", "json")
	viewOut, err := viewCmd.CombinedOutput()
	if err != nil {
		return "", nil
	}
	var payload struct {
		HeadPipeline struct {
			ID int `json:"id"`
		} `json:"head_pipeline"`
	}
	if trimmed := bytesTrimToJSON(viewOut); len(trimmed) == 0 || json.Unmarshal(trimmed, &payload) != nil || payload.HeadPipeline.ID == 0 {
		return "", nil
	}
	jobsCmd := h.cmd(ctx, "glab", h.pipelineJobsArgs(payload.HeadPipeline.ID)...)
	jobsOut, err := jobsCmd.CombinedOutput()
	if err != nil {
		return "", nil
	}
	jobID := findFailedJobID(jobsOut, failingNames)
	if jobID == 0 {
		return "", nil
	}
	traceCmd := h.cmd(ctx, "glab", "ci", "trace", fmt.Sprintf("%d", jobID))
	traceOut, _ := traceCmd.Output()
	return strings.TrimSpace(string(traceOut)), nil
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
// It returns whatever was parsed plus a non-nil error when a document was
// malformed: io.EOF terminates the stream cleanly, but any other decode error
// means a corrupt page, which the caller can surface instead of mistaking it
// for an empty result.
func decodeGitlabJobs(out []byte) ([]gitlabJob, error) {
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, nil
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
		var asArray []gitlabJob
		if err := json.Unmarshal(raw, &asArray); err == nil && len(asArray) > 0 {
			jobs = append(jobs, asArray...)
			continue
		}
		var asObject struct {
			Jobs []gitlabJob `json:"jobs"`
		}
		if err := json.Unmarshal(raw, &asObject); err == nil && len(asObject.Jobs) > 0 {
			jobs = append(jobs, asObject.Jobs...)
		}
	}
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
			Name:        job.Name,
			Bucket:      gitlabStatusBucket(job.Status),
			CompletedAt: job.completedAt(),
		})
	}
	return checks
}

func findFailedJobID(out []byte, failingNames []string) int {
	targets := map[string]struct{}{}
	for _, name := range failingNames {
		name = strings.TrimSpace(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	// Best effort: scan whatever jobs parsed; a corrupt later page does not
	// prevent locating a failed job that already decoded.
	jobs, _ := decodeGitlabJobs(out)
	for _, job := range jobs {
		if !strings.EqualFold(job.Status, "failed") {
			continue
		}
		if _, ok := targets[job.Name]; ok || len(targets) == 0 {
			return job.ID
		}
	}
	return 0
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

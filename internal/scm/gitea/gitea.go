// Package gitea implements scm.Host backed by the tea CLI (Gitea's official
// CLI).
//
// Unlike gh/glab, tea has no notion of "the current instance": it infers
// context from the working directory's git remote, which the daemon's
// detached bare-gate repo does not have. Every invocation here therefore
// carries --login and --repo explicitly (see the New doc comment).
//
// `tea actions runs view --jobs --output json` was empirically verified
// (against a real tea 0.15.1 CLI and a real Gitea 1.27.2 + Actions instance)
// to NOT emit clean structured per-job JSON: --output json only formats the
// run-level header block as JSON-ish text mixed with a genuine JSON jobs
// array, and that jobs array carries no `conclusion` field (only `status`),
// so pass/fail cannot be read from it. The REST job endpoint
// (GET /repos/{owner}/{repo}/actions/runs/{run}/jobs) does carry a `status` +
// `conclusion` pair (mirroring GitHub Actions' schema) and is reachable
// through `tea api`, which reuses tea's own stored login/token - no separate
// HTTP client or token configuration is needed. GetChecks,
// FetchFailedCheckLogs, and SetPRBaseBranch therefore go through `tea api`
// (the last because `tea pulls edit` has no `--base` flag) while every other
// operation (PR lifecycle, auth, run discovery) uses tea's own porcelain
// subcommands.
package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to Gitea through the tea CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	host         string // repo's Gitea hostname; used only for error messages
	login        string // tea login name configured for this host; scopes every tea invocation
	repoSlug     string // "owner/repo" slug tea's --repo flag expects
}

// New builds a Host. cliAvailable reports whether the tea binary is
// resolvable on the caller's PATH. host is the repo's Gitea hostname, used
// only to produce an actionable error message. login is the name of the tea
// login (from tea's own config.yml, see scm.ResolveGiteaLogin) configured for
// that host: tea requires --login on every command run outside a directory
// whose git remote it recognizes, which the daemon's detached bare-gate repo
// never is. repoSlug is the "owner/repo" path tea's --repo flag expects (no
// host prefix; --login already disambiguates the instance). All are optional;
// an empty login makes Available fail closed with a setup hint instead of
// silently falling back to tea's own default-login guessing.
func New(cmd CmdFactory, cliAvailable func() bool, host, login, repoSlug string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		host:         strings.TrimSpace(host),
		login:        strings.TrimSpace(login),
		repoSlug:     strings.TrimSpace(repoSlug),
	}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitea }

func (h *Host) Capabilities() scm.Capabilities {
	// MergeableState is declined: Gitea's PR `mergeable` field has a
	// documented upstream reliability bug (go-gitea/gitea#25849) that can
	// stick `false` after a conflict is actually resolved. Trusting it is
	// worse than declining the capability, matching Bitbucket's posture.
	return scm.Capabilities{MergeableState: false, FailedCheckLogs: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("tea CLI is not installed")
	}
	if h.login == "" {
		host := h.host
		if host == "" {
			host = "<gitea-host>"
		}
		return fmt.Errorf("no tea login configured for %s; run `tea logins add --url https://%s --token <token> --name <name>`", host, host)
	}
	// `tea whoami` has no --login flag, so it cannot be scoped to a specific
	// host the way `glab auth status --hostname` can. `tea api` accepts
	// --login on every call, so an authenticated GET /user through it is the
	// host-scoped equivalent: it fails when this login's token is missing or
	// invalid without being influenced by any other configured tea login.
	cmd := h.cmd(ctx, "tea", "api", "--login", h.login, "/user")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tea CLI is not authenticated for login %q", h.login)
	}
	return nil
}

type giteaPRListItem struct {
	Index string `json:"index"`
	State string `json:"state"`
	URL   string `json:"url"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	// tea pulls list has no --head/--base filter; the requested fields are
	// filtered client-side. --state is left at its default (open), matching
	// every other provider's FindPR: only an open PR for the branch counts.
	args := []string{"pulls", "list", "--repo", h.repoSlug, "--login", h.login, "--fields", "index,title,state,url,head,base", "--output", "json"}
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tea pulls list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		// Nonempty output with no JSON delimiter is not a legitimate "no
		// open PRs" response (an empty list still prints "[]", which has a
		// delimiter) - it must surface as an error rather than be read as
		// absence, which would otherwise cause the PR step to attempt a
		// duplicate create or report a misleading creation failure.
		return nil, fmt.Errorf("tea pulls list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var items []giteaPRListItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("tea pulls list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	base = strings.TrimSpace(base)
	for _, item := range items {
		if item.Head != branch {
			continue
		}
		if base != "" && item.Base != base {
			continue
		}
		return &scm.PR{Number: item.Index, URL: item.URL, BaseBranch: strings.TrimSpace(item.Base)}, nil
	}
	return nil, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := []string{"pulls", "create",
		"--repo", h.repoSlug,
		"--login", h.login,
		"--head", branch,
		"--base", base,
		"--title", content.Title,
		"--description", content.Body,
	}
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tea pulls create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// tea pulls create has no --output json flag, so the robust path is to
	// re-list the PR by head branch rather than scrape the human-readable
	// create output, which echoes the PR body and can itself contain
	// http(s) URLs that would confuse a naive "first URL line" scan.
	if pr, ferr := h.FindPR(ctx, branch, base); ferr == nil && pr != nil {
		return pr, nil
	}
	url := extractGiteaPRURL(out)
	if url == "" {
		return nil, fmt.Errorf("tea pulls create: could not determine PR URL from output: %s", strings.TrimSpace(string(out)))
	}
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
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
	args := []string{"pulls", "edit", id,
		"--repo", h.repoSlug,
		"--login", h.login,
		"--title", content.Title,
		"--description", content.Body,
	}
	if out, err := h.cmd(ctx, "tea", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("tea pulls edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

// SetPRBaseBranch retargets an existing PR. `tea pulls edit` (tea 0.15.1) has
// no `--base` flag even though `tea pulls create` does; inventing one would
// fail closed at the CLI. The Gitea REST edit-pull endpoint accepts `base`,
// and `tea api --method PATCH --field` reuses the stored login the same way
// job lookups do. `--method PATCH` is required: a body without an explicit
// method defaults tea api to POST.
func (h *Host) SetPRBaseBranch(ctx context.Context, pr *scm.PR, baseBranch string) error {
	id, err := giteaPRNumber(pr)
	if err != nil {
		return err
	}
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok {
		return fmt.Errorf("gitea: invalid repo slug %q", h.repoSlug)
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls/%s", owner, repo, id)
	args := []string{"api", "--login", h.login, "--method", "PATCH", "--field", "base=" + baseBranch, endpoint}
	if out, err := h.cmd(ctx, "tea", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tea api pull edit --base: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func giteaPRNumber(pr *scm.PR) (string, error) {
	if pr != nil {
		if id := strings.TrimSpace(pr.Number); id != "" {
			return id, nil
		}
		if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
			return num, nil
		}
	}
	return "", fmt.Errorf("pull request identity is required to retarget")
}

type giteaPRView struct {
	Index     int    `json:"index"`
	State     string `json:"state"`
	URL       string `json:"url"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	HeadSHA   string `json:"headSha"`
	Mergeable bool   `json:"mergeable"`
	HasMerged bool   `json:"hasMerged"`
}

func (h *Host) viewPR(ctx context.Context, number string) (giteaPRView, error) {
	args := []string{"pulls", strings.TrimSpace(number), "--repo", h.repoSlug, "--login", h.login, "--output", "json"}
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return giteaPRView{}, fmt.Errorf("tea pulls view: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return giteaPRView{}, fmt.Errorf("tea pulls view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var view giteaPRView
	if err := json.Unmarshal(trimmed, &view); err != nil {
		return giteaPRView{}, fmt.Errorf("tea pulls view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return view, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	view, err := h.viewPR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	// Gitea reports state="closed" AND hasMerged=true for a merged PR; check
	// hasMerged first so a merged PR is never read as plain CLOSED.
	if view.HasMerged {
		return scm.PRStateMerged, nil
	}
	return normalizeGiteaPRState(view.State), nil
}

// GetMergeableState is unsupported: see the Capabilities doc comment.
func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	return "", scm.ErrUnsupported
}

type giteaRunSummary struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Branch string `json:"branch"`
	Event  string `json:"event"`
}

func (h *Host) listRuns(ctx context.Context, branch string) ([]giteaRunSummary, error) {
	args := []string{"actions", "runs", "list", "--repo", h.repoSlug, "--login", h.login, "--branch", branch, "--output", "json"}
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tea actions runs list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var runs []giteaRunSummary
	if err := json.Unmarshal(trimmed, &runs); err != nil {
		return nil, fmt.Errorf("tea actions runs list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return runs, nil
}

type giteaJob struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	HeadSHA     string `json:"head_sha"`
	HTMLURL     string `json:"html_url"`
	CompletedAt string `json:"completed_at"`
}

// completedAt parses the job's completed_at timestamp (RFC3339, GitHub-Actions-
// schema style), returning the zero time when absent or unparseable.
func (j giteaJob) completedAt() time.Time {
	if strings.TrimSpace(j.CompletedAt) == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, j.CompletedAt); err == nil {
		return parsed
	}
	return time.Time{}
}

func (h *Host) runJobs(ctx context.Context, runID string) ([]giteaJob, error) {
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok {
		return nil, fmt.Errorf("gitea: invalid repo slug %q", h.repoSlug)
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/actions/runs/%s/jobs", owner, repo, runID)
	out, err := h.cmd(ctx, "tea", "api", "--login", h.login, endpoint).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tea api actions runs jobs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var resp struct {
		Jobs []giteaJob `json:"jobs"`
	}
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, fmt.Errorf("tea api actions runs jobs: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return resp.Jobs, nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	view, err := h.viewPR(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(view.Head) == "" {
		return nil, nil
	}
	runs, err := h.listRuns(ctx, view.Head)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	_, jobs, err := h.runJobsMatchingHeadSHA(ctx, runs, view.HeadSHA)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	return jobsToChecks(jobs), nil
}

// runsByIDDesc orders runs by highest numeric ID first rather than trusting
// `tea actions runs list`'s array order, which is not documented as
// newest-first. A run whose ID does not parse as an integer sorts last so it
// can never mask a numerically comparable run.
func runsByIDDesc(runs []giteaRunSummary) []giteaRunSummary {
	ordered := make([]giteaRunSummary, len(runs))
	copy(ordered, runs)
	sort.SliceStable(ordered, func(i, j int) bool {
		idI, okI := parseGiteaRunID(ordered[i].ID)
		idJ, okJ := parseGiteaRunID(ordered[j].ID)
		if okI && okJ {
			return idI > idJ
		}
		return okI && !okJ
	})
	return ordered
}

// runJobsMatchingHeadSHA finds the run (and its jobs) whose jobs belong to
// headSHA, searching candidate runs from highest ID to lowest. A branch can
// have more than one run - e.g. a manual re-run of an older commit can land a
// higher run ID than a newer commit's own run - so trusting the highest-ID
// run alone (or list order) could silently attribute a stale or unrelated
// commit's checks/logs to the PR's current head, or miss a matching lower-ID
// run entirely. When headSHA is empty there is nothing to match against, so
// only the highest-ID run is considered, preserving prior best-effort
// behavior.
func (h *Host) runJobsMatchingHeadSHA(ctx context.Context, runs []giteaRunSummary, headSHA string) (giteaRunSummary, []giteaJob, error) {
	ordered := runsByIDDesc(runs)
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		run := ordered[0]
		jobs, err := h.runJobs(ctx, run.ID)
		return run, jobs, err
	}
	for _, run := range ordered {
		jobs, err := h.runJobs(ctx, run.ID)
		if err != nil {
			return giteaRunSummary{}, nil, err
		}
		if anyJobMatchesHeadSHA(jobs, headSHA) {
			return run, jobs, nil
		}
	}
	// No run yet matches the PR's current head commit: CI has not caught up
	// with the latest push, or only stale/unrelated runs exist so far.
	return giteaRunSummary{}, nil, nil
}

func parseGiteaRunID(id string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func anyJobMatchesHeadSHA(jobs []giteaJob, sha string) bool {
	for _, j := range jobs {
		if j.HeadSHA == sha {
			return true
		}
	}
	return false
}

func jobsToChecks(jobs []giteaJob) []scm.Check {
	checks := make([]scm.Check, 0, len(jobs))
	for _, job := range jobs {
		checks = append(checks, scm.Check{
			Name:        job.Name,
			Bucket:      giteaStatusBucket(job.Status, job.Conclusion),
			CompletedAt: job.completedAt(),
			Link:        job.HTMLURL,
		})
	}
	return checks
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, headSHA string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	view, err := h.viewPR(ctx, pr.Number)
	if err != nil || strings.TrimSpace(view.Head) == "" {
		return "", nil
	}
	runs, err := h.listRuns(ctx, view.Head)
	if err != nil || len(runs) == 0 {
		return "", nil
	}
	matchSHA := strings.TrimSpace(headSHA)
	if matchSHA == "" {
		matchSHA = view.HeadSHA
	}
	run, jobs, err := h.runJobsMatchingHeadSHA(ctx, runs, matchSHA)
	if err != nil || run.ID == "" {
		return "", nil
	}
	jobID := findFailedGiteaJobID(jobs, failingNames)
	if jobID == 0 {
		return "", nil
	}
	logsCmd := h.cmd(ctx, "tea", "actions", "runs", "logs", run.ID,
		"--job", strconv.Itoa(jobID),
		"--repo", h.repoSlug,
		"--login", h.login,
	)
	out, _ := logsCmd.Output()
	return stripGiteaLogsHeader(string(out)), nil
}

func findFailedGiteaJobID(jobs []giteaJob, failingNames []string) int {
	targets := map[string]struct{}{}
	for _, name := range failingNames {
		name = strings.TrimSpace(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	for _, job := range jobs {
		if giteaStatusBucket(job.Status, job.Conclusion) != scm.CheckBucketFail {
			continue
		}
		if _, ok := targets[job.Name]; ok || len(targets) == 0 {
			return job.ID
		}
	}
	return 0
}

// stripGiteaLogsHeader removes the "Logs for job N:\n---\n" banner that `tea
// actions runs logs` prints before the actual log body.
func stripGiteaLogsHeader(s string) string {
	if idx := strings.Index(s, "---\n"); idx >= 0 {
		return strings.TrimSpace(s[idx+len("---\n"):])
	}
	return strings.TrimSpace(s)
}

// splitOwnerRepo splits a "owner/repo" slug (Gitea has no nested subgroups,
// unlike GitLab) into its two path segments.
func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	slug = strings.Trim(strings.TrimSpace(slug), "/")
	if slug == "" {
		return "", "", false
	}
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// bytesTrimToJSON skips any banner text before the first '{' or '[', mirroring
// the same defensive scan the gitlab CLI-shelled Host uses: a version-check
// notice or similar stray line before the JSON payload must not break parsing.
func bytesTrimToJSON(out []byte) []byte {
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

// extractGiteaPRURL scans out (tea pulls create's human-readable stdout) from
// the end for the last line that is a bare http(s) URL - the trailing summary
// line `tea` always prints after creating a PR. It is a fallback for
// CreatePR only; the primary path re-lists the PR for a structured result.
func extractGiteaPRURL(out []byte) string {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if (strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) && !strings.ContainsAny(line, " \t") {
			return line
		}
	}
	return ""
}

func giteaStatusBucket(status, conclusion string) scm.CheckBucket {
	if strings.ToLower(strings.TrimSpace(status)) != "completed" {
		return scm.CheckBucketPending
	}
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "success":
		return scm.CheckBucketPass
	case "failure":
		return scm.CheckBucketFail
	case "cancelled", "canceled":
		return scm.CheckBucketCancel
	case "skipped":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}

func normalizeGiteaPRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "open":
		return scm.PRStateOpen
	case "closed":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(raw))
	}
}

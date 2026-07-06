// Package github implements scm.Host backed by the gh CLI.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to GitHub through the gh CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	host         string // repo's GitHub hostname; scopes the auth check
	repo         string // "owner/name" slug for --repo; empty when unknown
	forkOwner    string // fork owner for cross-repository PR heads
}

// New builds a Host. cliAvailable reports whether the gh binary is
// resolvable on the caller's PATH (possibly overridden by env). host is the
// repo's GitHub hostname; when set the availability check is scoped to it via
// --hostname so a stale credential for an unrelated configured gh host cannot
// make this repo look unauthenticated. repo is the "owner/name" slug; when set
// it is passed via --repo to every PR/run command so they resolve the right
// repository regardless of the process working directory. The daemon runs from
// a fixed, non-repo working dir, so without this gh cannot infer the repo (or
// branch) and fails on every poll. host is optional; empty reproduces the
// legacy unscoped auth-check behavior.
func New(cmd CmdFactory, cliAvailable func() bool, host, repo string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		host:         strings.TrimSpace(host),
		repo:         strings.TrimSpace(repo),
	}
}

// NewWithFork builds a Host that opens PRs on repo using forkRepo as the head
// repository owner. forkRepo is an "owner/name" slug; only the owner is needed
// because gh pr create expects --head <owner>:<branch>. host is optional; see
// New for its role in scoping the auth check.
func NewWithFork(cmd CmdFactory, cliAvailable func() bool, host, repo, forkRepo string) *Host {
	h := New(cmd, cliAvailable, host, repo)
	h.forkOwner = repoOwner(forkRepo)
	return h
}

// RepoSlug extracts the "owner/name" identifier from a GitHub remote or PR URL.
// It supports https URLs, scp-style ssh URLs (git@github.com:owner/name.git),
// ssh:// URLs, and longer paths such as PR links (the leading two path segments
// are used). It returns "" when the input has no owner/name pair.
func RepoSlug(remoteURL string) string {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return ""
	}
	raw = strings.TrimSuffix(raw, ".git")

	// Reduce raw to the path portion after the host.
	switch {
	case strings.Contains(raw, "://"):
		rest := raw[strings.Index(raw, "://")+len("://"):]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return ""
		}
		raw = rest[slash+1:]
	case strings.Contains(raw, ":"):
		// scp-style ssh: [user@]host:owner/name
		raw = raw[strings.IndexByte(raw, ':')+1:]
	}

	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	owner, name := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

// HostPrefixedSlug returns "host/owner/name" for GitHub Enterprise Server
// instances and plain "owner/name" for github.com. This is the format that
// the gh CLI's --repo flag requires for GHE.
func HostPrefixedSlug(remoteURL string) string {
	slug := RepoSlug(remoteURL)
	if slug == "" {
		return ""
	}
	host := scm.ExtractHost(remoteURL)
	if host == "" || strings.EqualFold(host, "github.com") {
		return slug
	}
	return host + "/" + slug
}

// repoArgs returns the --repo flag pair when the slug is known, so gh commands
// resolve the right repository regardless of the process working directory.
func (h *Host) repoArgs() []string {
	if h.repo == "" {
		return nil
	}
	return []string{"--repo", h.repo}
}

func (h *Host) headRef(branch string) string {
	if h.forkOwner == "" {
		return branch
	}
	return h.forkOwner + ":" + branch
}

// isJSONArray reports whether b is a valid JSON array (possibly empty).
func isJSONArray(b []byte) bool {
	var probe []json.RawMessage
	return json.Unmarshal(b, &probe) == nil
}

func repoOwner(slug string) string {
	owner, _, ok := strings.Cut(strings.TrimSpace(slug), "/")
	if !ok {
		return ""
	}
	return strings.TrimSpace(owner)
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitHub }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("gh CLI is not installed")
	}
	// Scope the auth check to this repo's host. Unscoped `gh auth status`
	// checks every authenticated account and exits non-zero if ANY of them has
	// a stale/expired token, even when this repo's own host is fully
	// authenticated. Passing --hostname keeps an unrelated bad credential from
	// poisoning availability for this repo. When the host is unknown we fall
	// back to the unscoped check (fail-safe: same behavior as before).
	authArgs := []string{"auth", "status"}
	if h.host != "" {
		authArgs = append(authArgs, "--hostname", h.host)
	}
	if err := h.cmd(ctx, "gh", authArgs...).Run(); err != nil {
		return errors.New("gh CLI is not authenticated")
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"pr", "list", "--head", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--base", base)
	}
	args = append(args, h.repoArgs()...)
	jsonFields := "number,url"
	if h.forkOwner != "" {
		jsonFields = "number,url,headRefName,headRepositoryOwner"
	}
	args = append(args, "--state", "open", "--json", jsonFields)
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var prs []struct {
		Number              int    `json:"number"`
		URL                 string `json:"url"`
		HeadRefName         string `json:"headRefName"`
		HeadRepositoryOwner *struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	if err := json.Unmarshal(out, &prs); err != nil || len(prs) == 0 {
		return nil, nil
	}
	for _, candidate := range prs {
		if !h.matchesHead(candidate.HeadRefName, candidate.HeadRepositoryOwner, branch) {
			continue
		}
		pr := &scm.PR{URL: strings.TrimSpace(candidate.URL)}
		if candidate.Number > 0 {
			pr.Number = fmt.Sprintf("%d", candidate.Number)
		} else if num, nerr := scm.ExtractPRNumber(pr.URL); nerr == nil {
			pr.Number = num
		}
		if pr.URL == "" {
			return nil, nil
		}
		return pr, nil
	}
	return nil, nil
}

func (h *Host) matchesHead(headRefName string, owner *struct {
	Login string `json:"login"`
}, branch string) bool {
	if h.forkOwner == "" {
		return true
	}
	if strings.TrimSpace(headRefName) != "" && headRefName != branch {
		return false
	}
	if owner == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(owner.Login), h.forkOwner)
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := append([]string{"pr", "create",
		"--head", h.headRef(branch),
		"--base", base,
	}, h.repoArgs()...)
	args = append(args, "--title", content.Title, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	url := strings.TrimSpace(string(out))
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := pr.Number
	if id == "" {
		id = pr.URL
	}
	args := append([]string{"pr", "edit", id}, h.repoArgs()...)
	args = append(args, "--title", content.Title, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gh pr edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	args := append([]string{"pr", "view", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "state", "--jq", ".state")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return normalizePRState(strings.TrimSpace(string(out))), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	args := append([]string{"pr", "checks", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "name,state,bucket,completedAt")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || (exitErr.ExitCode() != 1 && exitErr.ExitCode() != 8) {
			return nil, fmt.Errorf("gh pr checks: %w", err)
		}
		if isUnsupportedJSONFlagError(exitErr.Stderr) {
			return h.getChecksLegacy(ctx, pr)
		}
		diagnostic := string(out) + string(exitErr.Stderr)
		if strings.Contains(diagnostic, "no checks reported") {
			return nil, nil
		}
		if !isJSONArray(out) {
			return nil, fmt.Errorf("gh pr checks: %w", err)
		}
	}
	var raw []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, r := range raw {
		var completedAt time.Time
		if r.CompletedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, r.CompletedAt); parseErr == nil {
				completedAt = parsed
			}
		}
		checks = append(checks, scm.Check{Name: r.Name, Bucket: normalizeCheckBucket(r.Bucket, r.State), CompletedAt: completedAt})
	}
	return checks, nil
}

// isUnsupportedJSONFlagError reports whether stderr indicates the installed gh
// CLI predates v2.46, where `pr checks` gained the --json flag.
func isUnsupportedJSONFlagError(stderr []byte) bool {
	msg := strings.ToLower(string(stderr))
	return strings.Contains(msg, "unknown flag") && strings.Contains(msg, "--json")
}

// getChecksLegacy reads checks through structured REST endpoints for gh CLIs
// older than v2.46. Unscoped hosts retain the tab-separated fallback used by
// legacy callers that do not provide a repository slug.
func (h *Host) getChecksLegacy(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	if h.repo != "" {
		return h.getChecksLegacyStructured(ctx, pr)
	}

	args := append([]string{"pr", "checks", pr.Number}, h.repoArgs()...)
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || (exitErr.ExitCode() != 1 && exitErr.ExitCode() != 8) {
			return nil, fmt.Errorf("gh pr checks: %w", err)
		}
		if strings.Contains(string(out)+string(exitErr.Stderr), "no checks reported") {
			return nil, nil
		}
		// Exits 1 (failures) and 8 (pending) still print the check table on
		// stdout; fall through and parse it. Anything unparseable fails below.
	}
	checks, parseErr := parseChecksTSV(out)
	if parseErr != nil || (err != nil && len(checks) == 0) {
		// Fail closed: a nonzero exit without a recognized diagnostic or a
		// parseable check table is a command failure, not an empty result.
		if err != nil {
			return nil, fmt.Errorf("gh pr checks: %w", err)
		}
		return nil, parseErr
	}
	return checks, nil
}

type legacyCheckRun struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completed_at"`
}

func (h *Host) getChecksLegacyStructured(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	headArgs := append([]string{"pr", "view", pr.Number}, h.repoArgs()...)
	headArgs = append(headArgs, "--json", "headRefOid")
	headOut, err := h.cmd(ctx, "gh", headArgs...).Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("gh pr view head: %w", err)
	}
	var head struct {
		OID string `json:"headRefOid"`
	}
	if err := json.Unmarshal(headOut, &head); err != nil {
		return nil, fmt.Errorf("parse PR head: %w", err)
	}
	head.OID = strings.TrimSpace(head.OID)
	if head.OID == "" {
		return nil, errors.New("parse PR head: empty headRefOid")
	}

	endpoint := fmt.Sprintf("repos/%s/commits/%s/check-runs", h.apiRepoSlug(), head.OID)
	runsOut, err := h.cmd(ctx, "gh", h.apiArgs(endpoint, "--paginate")...).Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("gh api check-runs: %w", err)
	}
	runs, err := decodeLegacyCheckRuns(runsOut)
	if err != nil {
		return nil, err
	}

	endpoint = fmt.Sprintf("repos/%s/commits/%s/status", h.apiRepoSlug(), head.OID)
	statusOut, err := h.cmd(ctx, "gh", h.apiArgs(endpoint)...).Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("gh api commit status: %w", err)
	}
	var statusPayload struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(statusOut, &statusPayload); err != nil {
		return nil, fmt.Errorf("parse commit statuses: %w", err)
	}

	checks := make([]scm.Check, 0, len(runs)+len(statusPayload.Statuses))
	for _, run := range runs {
		checks = append(checks, scm.Check{
			Name:        run.Name,
			Bucket:      legacyCheckRunBucket(run.Status, run.Conclusion),
			CompletedAt: parseGitHubTime(run.CompletedAt),
		})
	}
	for _, status := range statusPayload.Statuses {
		checks = append(checks, scm.Check{
			Name:   status.Context,
			Bucket: normalizeCheckBucket("", status.State),
		})
	}
	return checks, nil
}

func (h *Host) apiRepoSlug() string {
	repo := strings.TrimSpace(h.repo)
	if h.host != "" {
		repo = strings.TrimPrefix(repo, strings.TrimSpace(h.host)+"/")
	}
	return repo
}

func (h *Host) apiArgs(endpoint string, extra ...string) []string {
	args := []string{"api"}
	if h.host != "" && !strings.EqualFold(h.host, "github.com") {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	return append(args, extra...)
}

func decodeLegacyCheckRuns(out []byte) ([]legacyCheckRun, error) {
	decoder := json.NewDecoder(bytes.NewReader(out))
	var runs []legacyCheckRun
	seenPayload := false
	for {
		var payload struct {
			CheckRuns []legacyCheckRun `json:"check_runs"`
		}
		err := decoder.Decode(&payload)
		if errors.Is(err, io.EOF) {
			if !seenPayload {
				return nil, errors.New("parse check-runs: empty response")
			}
			return runs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse check-runs: %w", err)
		}
		seenPayload = true
		runs = append(runs, payload.CheckRuns...)
	}
}

func legacyCheckRunBucket(status, conclusion string) scm.CheckBucket {
	if !strings.EqualFold(strings.TrimSpace(status), "completed") {
		return scm.CheckBucketPending
	}
	if bucket := normalizeCheckBucket("", conclusion); bucket != "" {
		return bucket
	}
	return scm.CheckBucketPending
}

func parseGitHubTime(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// parseChecksTSV parses the plain `gh pr checks` table: one check per line,
// tab-separated, with the name in column 1 and the bucket word (pass, fail,
// pending, skipping, cancel) in column 2.
func parseChecksTSV(out []byte) ([]scm.Check, error) {
	checks := []scm.Check{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			return nil, fmt.Errorf("gh pr checks: unexpected output line %q", line)
		}
		bucket := scm.CheckBucket(strings.TrimSpace(fields[1]))
		switch bucket {
		case scm.CheckBucketPass, scm.CheckBucketFail, scm.CheckBucketPending, scm.CheckBucketCancel, scm.CheckBucketSkip:
		default:
			return nil, fmt.Errorf("gh pr checks: unexpected check state %q in line %q", fields[1], line)
		}
		checks = append(checks, scm.Check{Name: strings.TrimSpace(fields[0]), Bucket: bucket})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	args := append([]string{"pr", "view", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "mergeable", "--jq", ".mergeable")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w", err)
	}
	return normalizeMergeableState(strings.TrimSpace(string(out))), nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, _ *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	targets := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		name = normalizeRunName(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	args := []string{"run", "list", "--branch", branch}
	if strings.TrimSpace(headSHA) != "" {
		args = append(args, "--commit", strings.TrimSpace(headSHA))
	}
	args = append(args, h.repoArgs()...)
	args = append(args,
		"--status", "failure",
		"--limit", "20",
		"--json", "databaseId,headSha,name,displayTitle,workflowName",
	)
	listCmd := h.cmd(ctx, "gh", args...)
	listOut, err := listCmd.Output()
	if err != nil {
		return "", nil
	}
	var runs []githubRun
	if err := json.Unmarshal(listOut, &runs); err != nil {
		return "", nil
	}
	for _, run := range runs {
		if !runMatchesTargets(ctx, h, run, targets) {
			continue
		}
		viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
		viewArgs = append(viewArgs, "--log-failed")
		viewCmd := h.cmd(ctx, "gh", viewArgs...)
		out, err := viewCmd.Output()
		if err != nil {
			continue
		}
		logs := strings.TrimSpace(string(out))
		if logs != "" {
			return logs, nil
		}
	}
	return "", nil
}

type githubRun struct {
	DatabaseID   int    `json:"databaseId"`
	HeadSHA      string `json:"headSha"`
	Name         string `json:"name"`
	DisplayTitle string `json:"displayTitle"`
	WorkflowName string `json:"workflowName"`
}

type githubRunView struct {
	Jobs []githubRunJob `json:"jobs"`
}

type githubRunJob struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
}

func runMatchesTargets(ctx context.Context, h *Host, run githubRun, targets map[string]struct{}) bool {
	for _, candidate := range []string{run.Name, run.DisplayTitle, run.WorkflowName} {
		if _, ok := targets[normalizeRunName(candidate)]; ok {
			return true
		}
	}
	if run.DatabaseID == 0 {
		return false
	}
	viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
	viewArgs = append(viewArgs, "--json", "jobs")
	viewCmd := h.cmd(ctx, "gh", viewArgs...)
	out, err := viewCmd.Output()
	if err != nil {
		return false
	}
	var payload githubRunView
	if err := json.Unmarshal(out, &payload); err != nil {
		return false
	}
	for _, job := range payload.Jobs {
		if !isFailedJob(job) {
			continue
		}
		if _, ok := targets[normalizeRunName(job.Name)]; ok {
			return true
		}
	}
	return false
}

func isFailedJob(job githubRunJob) bool {
	state := strings.ToUpper(strings.TrimSpace(job.Conclusion))
	if state == "" {
		state = strings.ToUpper(strings.TrimSpace(job.Status))
	}
	switch state {
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return true
	default:
		return false
	}
}

func normalizeRunName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OPEN":
		return scm.PRStateOpen
	case "MERGED":
		return scm.PRStateMerged
	case "CLOSED":
		return scm.PRStateClosed
	default:
		return scm.PRState(raw)
	}
}

func normalizeMergeableState(raw string) scm.MergeableState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MERGEABLE":
		return scm.MergeableOK
	case "CONFLICTING":
		return scm.MergeableConflict
	case "UNKNOWN", "":
		return scm.MergeablePending
	default:
		return scm.MergeableState(raw)
	}
}

func normalizeCheckBucket(bucket, state string) scm.CheckBucket {
	if normalized := scm.CheckBucket(strings.TrimSpace(bucket)); normalized != "" {
		return normalized
	}

	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return scm.CheckBucketPass
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return scm.CheckBucketFail
	case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "EXPECTED":
		return scm.CheckBucketPending
	case "CANCELLED":
		return scm.CheckBucketCancel
	case "SKIPPED", "NEUTRAL", "STALE":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}

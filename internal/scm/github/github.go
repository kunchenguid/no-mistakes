// Package github implements scm.Host backed by the gh CLI.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
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

	// checksJSONUnsupported is set after an older gh rejects `pr checks
	// --json`, so later polls use the compatible status rollup directly.
	checksJSONUnsupported atomic.Bool
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
	return HostPrefixedSlugForHost(remoteURL, scm.ExtractHost(remoteURL))
}

// HostPrefixedSlugForHost is HostPrefixedSlug using an already-resolved host.
// This lets callers honor SSH HostName aliases without rewriting the remote.
func HostPrefixedSlugForHost(remoteURL, host string) string {
	slug := RepoSlug(remoteURL)
	if slug == "" {
		return ""
	}
	host = strings.ToLower(strings.TrimSpace(host))
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

// prSelector returns the explicit gh PR selector for pr, preferring the numeric
// PR number and falling back to the canonical PR URL; both name the exact pull
// request to gh regardless of the process working directory. It fails closed
// when neither is known: an empty positional makes `gh pr <verb>` fall back to
// resolving the current branch of the cwd, and the daemon runs from a detached
// bare gate repo whose HEAD is the default branch (main), so an inferred
// selector silently targets the wrong PR (or none — "no pull requests found for
// branch main") instead of the feature PR the pipeline already knows.
func prSelector(pr *scm.PR) (string, error) {
	if pr != nil {
		if n := strings.TrimSpace(pr.Number); n != "" {
			return n, nil
		}
		if u := strings.TrimSpace(pr.URL); u != "" {
			return u, nil
		}
	}
	return "", errors.New("no PR number or URL known; refusing to run gh with a cwd-inferred branch")
}

func (h *Host) headRef(branch string) string {
	if h.forkOwner == "" {
		return branch
	}
	return h.forkOwner + ":" + branch
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
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	args := append([]string{"pr", "edit", selector}, h.repoArgs()...)
	args = append(args, "--title", content.Title, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gh pr edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "state", "--jq", ".state")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", ghCLIError("gh pr view", err)
	}
	return normalizePRState(strings.TrimSpace(string(out))), nil
}

// GetChecks prefers the structured `gh pr checks --json` interface. GitHub
// CLI added that flag in v2.50.0; older packaged versions reject it before
// making a request, so the Host detects that one capability error and uses the
// older `gh pr view --json statusCheckRollup` interface for the rest of the run.
func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	if h.checksJSONUnsupported.Load() {
		return h.getChecksViaStatusRollup(ctx, selector)
	}
	args := append([]string{"pr", "checks", selector}, h.repoArgs()...)
	args = append(args, "--json", "name,state,bucket,completedAt")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := commandStderr(err)
		if strings.Contains(string(out)+string(stderr), "no checks reported") {
			return nil, nil
		}
		if strings.Contains(string(stderr), "unknown flag: --json") {
			h.checksJSONUnsupported.Store(true)
			return h.getChecksViaStatusRollup(ctx, selector)
		}
		return nil, ghCLIError("gh pr checks", err)
	}
	return parseChecksJSON(out)
}

// getChecksViaStatusRollup preserves structured states and completion times on
// gh versions that predate `pr checks --json`. The rollup is a GraphQL union of
// CheckRun and StatusContext values, which expose different name/state fields.
func (h *Host) getChecksViaStatusRollup(ctx context.Context, selector string) ([]scm.Check, error) {
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "statusCheckRollup")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return nil, ghCLIError("gh pr view statusCheckRollup", err)
	}
	var payload struct {
		StatusCheckRollup []struct {
			Typename    string `json:"__typename"`
			Name        string `json:"name"`
			Context     string `json:"context"`
			Conclusion  string `json:"conclusion"`
			Status      string `json:"status"`
			State       string `json:"state"`
			CompletedAt string `json:"completedAt"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	checks := make([]scm.Check, 0, len(payload.StatusCheckRollup))
	for _, item := range payload.StatusCheckRollup {
		name, state := item.Name, item.Conclusion
		if item.Typename == "StatusContext" {
			name, state = item.Context, item.State
		} else if state == "" {
			state = item.Status
		}
		checks = append(checks, scm.Check{
			Name:        name,
			Bucket:      normalizedCheckBucketOrPending("", state),
			CompletedAt: parseCheckCompletedAt(item.CompletedAt),
		})
	}
	return checks, nil
}

func parseChecksJSON(out []byte) ([]scm.Check, error) {
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
		checks = append(checks, scm.Check{
			Name:        r.Name,
			Bucket:      normalizedCheckBucketOrPending(r.Bucket, r.State),
			CompletedAt: parseCheckCompletedAt(r.CompletedAt),
		})
	}
	return checks, nil
}

// commandStderr returns stderr captured by exec.Cmd.Output for a failed child.
func commandStderr(err error) []byte {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Stderr
	}
	return nil
}

func ghCLIError(op string, err error) error {
	if detail := compactCLIError(commandStderr(err)); detail != "" {
		return fmt.Errorf("%s: %w: %s", op, err, detail)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// compactCLIError keeps provider warnings useful without allowing raw,
// credentialled, or unbounded subprocess output into the CI step log.
func compactCLIError(stderr []byte) string {
	const maxLen = 200
	for _, line := range strings.Split(string(stderr), "\n") {
		line = safeurl.RedactText(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if len(line) > maxLen {
			cut := maxLen
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut]
		}
		return line
	}
	return ""
}

func parseCheckCompletedAt(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// normalizedCheckBucketOrPending fails safe when GitHub adds a state: an
// unknown value must keep the monitor waiting rather than look successful.
func normalizedCheckBucketOrPending(bucket, state string) scm.CheckBucket {
	if normalized := normalizeCheckBucket(bucket, state); normalized != "" {
		return normalized
	}
	return scm.CheckBucketPending
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "mergeable", "--jq", ".mergeable")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", ghCLIError("gh pr view mergeable", err)
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
	switch normalized := scm.CheckBucket(strings.TrimSpace(bucket)); normalized {
	case scm.CheckBucketPass, scm.CheckBucketFail, scm.CheckBucketPending,
		scm.CheckBucketCancel, scm.CheckBucketSkip:
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

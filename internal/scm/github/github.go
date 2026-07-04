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

	// checksJSONUnsupported memoizes that `gh pr checks --json` isn't
	// supported by the resolved gh binary (added in gh v2.50.0, cli/cli#9079),
	// so GetChecks stops retrying it and goes straight to the
	// statusCheckRollup fallback. Attempted at most once per process.
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
		return "", fmt.Errorf("gh pr view: %w: %s", err, compactCLIError(exitStderr(err)))
	}
	return normalizePRState(strings.TrimSpace(string(out))), nil
}

// GetChecks reports each check's normalized state for pr. On gh >= 2.50.0 it
// runs `gh pr checks --json` directly. On older gh, that flag doesn't exist
// (cli/cli#9079 added it in 2.50.0); the first time that's detected via its
// stderr ("unknown flag: --json"), it's memoized on h so every later call on
// this Host goes straight to the statusCheckRollup fallback instead of
// re-attempting a flag that will never work until the daemon restarts.
func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	if !h.checksJSONUnsupported.Load() {
		checks, err, unsupported := h.getChecksViaFlag(ctx, pr)
		if !unsupported {
			return checks, err
		}
		h.checksJSONUnsupported.Store(true)
	}
	return h.getChecksViaStatusRollup(ctx, pr)
}

// getChecksViaFlag runs `gh pr checks --json`. unsupported is true only when
// gh rejected --json itself (stderr "unknown flag: --json"), signaling the
// caller should fall back rather than treat this as a real error.
func (h *Host) getChecksViaFlag(ctx context.Context, pr *scm.PR) (checks []scm.Check, err error, unsupported bool) {
	args := append([]string{"pr", "checks", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "name,state,bucket,completedAt")
	cmd := h.cmd(ctx, "gh", args...)
	out, cmdErr := cmd.Output()
	if cmdErr != nil {
		stderr := exitStderr(cmdErr)
		if strings.Contains(string(out)+string(stderr), "no checks reported") {
			return nil, nil, false
		}
		if strings.Contains(string(stderr), "unknown flag: --json") {
			return nil, nil, true
		}
		return nil, fmt.Errorf("gh pr checks: %w: %s", cmdErr, compactCLIError(stderr)), false
	}
	checks, err = parseChecksJSON(out)
	return checks, err, false
}

// getChecksViaStatusRollup is the compatibility fallback for gh < 2.50.0: it
// reads the same information through `gh pr view --json statusCheckRollup`,
// which has been stable since long before `pr checks --json` existed and
// exits 0 with well-formed JSON regardless of check state (unlike `pr
// checks`, which uses nonzero exits to signal pending/failing checks).
func (h *Host) getChecksViaStatusRollup(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	args := append([]string{"pr", "view", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "statusCheckRollup")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr view statusCheckRollup: %w: %s", err, compactCLIError(exitStderr(err)))
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
		// normalizeCheckBucket's default is the EMPTY bucket, which
		// downstream counts as neither Failing() nor Pending() and so
		// is treated as PASSED (ci.go's aggregation default branch).
		// An unrecognized future state must keep polling, never
		// silently green-light the merge gate.
		bucket := normalizeCheckBucket("", state)
		if bucket == "" {
			bucket = scm.CheckBucketPending
		}
		var completedAt time.Time
		if item.CompletedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, item.CompletedAt); parseErr == nil {
				completedAt = parsed
			}
		}
		checks = append(checks, scm.Check{Name: name, Bucket: bucket, CompletedAt: completedAt})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	args := append([]string{"pr", "view", pr.Number}, h.repoArgs()...)
	args = append(args, "--json", "mergeable", "--jq", ".mergeable")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w: %s", err, compactCLIError(exitStderr(err)))
	}
	return normalizeMergeableState(strings.TrimSpace(string(out))), nil
}

// parseChecksJSON decodes the `gh pr checks --json name,state,bucket,completedAt`
// payload into normalized checks.
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

// exitStderr returns the child process's captured stderr when err is an
// *exec.ExitError, or nil otherwise. Output() only populates ExitError.Stderr
// when cmd.Stderr was nil at Start time (true for every gh invocation here).
func exitStderr(err error) []byte {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Stderr
	}
	return nil
}

// compactCLIError collapses a gh CLI stderr capture to its first non-empty
// line, capped to keep ci.log one line per poll.
func compactCLIError(stderr []byte) string {
	const maxLen = 200
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxLen {
			line = line[:maxLen]
		}
		return line
	}
	return ""
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

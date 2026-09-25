// Package bitbucket implements scm.Host backed by the twg CLI (Atlassian's
// Teamwork Graph CLI), which owns Bitbucket Cloud authentication itself the
// same way gh/glab/tea own auth for their providers. This replaces an earlier
// hand-rolled HTTP client that read a Bitbucket Cloud API token out of
// NO_MISTAKES_BITBUCKET_EMAIL/NO_MISTAKES_BITBUCKET_API_TOKEN: those variables
// lived in the daemon's process environment, which every spawned agent
// subprocess inherits in full (see internal/runenv.Overlay.Apply and
// internal/agent/env.go) unless explicitly scrubbed, and nothing scrubbed
// them. Shelling out to twg means no Bitbucket secret ever needs to enter
// no-mistakes' own environment at all.
//
// twg has no raw Bitbucket Cloud API escape hatch (`twg api` explicitly
// refuses api.bitbucket.org endpoints), so every operation here goes through
// `twg bb ...` porcelain subcommands, mirroring gitea.go's posture toward
// tea. `--workspace`/`--repo` are passed explicitly on every invocation
// (never left to twg's git-remote auto-detection) because the daemon's
// detached bare-gate repo does not have a real Bitbucket git remote, the same
// reason gitea.go always carries --login/--repo.
package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// RepoRef identifies a Bitbucket Cloud repository by workspace and repo slug.
type RepoRef struct {
	Workspace string
	RepoSlug  string
}

// ParseRepoRef extracts a workspace/repo slug pair from a Bitbucket Cloud git
// remote or web URL (bitbucket.org or the scp-like git@bitbucket.org: form).
func ParseRepoRef(raw string) (RepoRef, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimSuffix(trimmed, ".git")

	if strings.HasPrefix(trimmed, "git@bitbucket.org:") {
		path := strings.TrimPrefix(trimmed, "git@bitbucket.org:")
		return parseRepoPath(path)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return RepoRef{}, fmt.Errorf("parse bitbucket repo URL: %w", err)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "bitbucket.org" && !strings.HasSuffix(host, ".bitbucket.org") {
		return RepoRef{}, fmt.Errorf("unsupported Bitbucket host %q", parsed.Host)
	}
	return parseRepoPath(strings.TrimPrefix(parsed.Path, "/"))
}

func parseRepoPath(path string) (RepoRef, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return RepoRef{}, fmt.Errorf("invalid Bitbucket repository path %q", path)
	}
	return RepoRef{Workspace: parts[0], RepoSlug: parts[1]}, nil
}

// CommitStatus mirrors one entry of Bitbucket's commit-status shape, as
// hydrated by `twg bb pull-requests get --statuses` under the `_statuses` key.
type CommitStatus struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	State       string `json:"state"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// Host talks to Bitbucket Cloud through the twg CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	repo         RepoRef
	draft        bool // open created PRs as drafts
}

// New builds a Host. cliAvailable reports whether the twg binary is
// resolvable on the caller's PATH. repo identifies the Bitbucket workspace
// and repository every invocation is scoped to. When draft is true, created
// PRs are opened as drafts.
func New(cmd CmdFactory, cliAvailable func() bool, repo RepoRef, draft bool) *Host {
	return &Host{cmd: cmd, cliAvailable: cliAvailable, repo: repo, draft: draft}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderBitbucket }

// Capabilities reports Bitbucket's feature matrix. Bitbucket's REST API does
// not expose a reliable merge-conflict probe, so MergeableState is off.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: false, FailedCheckLogs: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("twg CLI is not installed")
	}
	cmd := h.cmd(ctx, "twg", "whoami")
	if err := cmd.Run(); err != nil {
		return errors.New("twg CLI is not authenticated; run `twg login`")
	}
	return nil
}

func (h *Host) repoArgs() []string {
	return []string{"--workspace", h.repo.Workspace, "--repo", h.repo.RepoSlug}
}

// bitbucketPullRequest is the subset of twg's `pull-requests get`/`query`/
// `create`/`update` JSON this package reads. Field names mirror the
// underlying Bitbucket Cloud API shape verbatim (id, state, links.html.href),
// which twg's own documented agent-field presets confirm it passes through.
type bitbucketPullRequest struct {
	ID    int    `json:"id"`
	State string `json:"state"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (pr bitbucketPullRequest) toPR(repo RepoRef) *scm.PR {
	return &scm.PR{
		Number: strconv.Itoa(pr.ID),
		URL:    prURL(repo, pr.ID, pr.Links.HTML.Href),
	}
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"bb", "pull-requests", "query", "--source", branch, "--state", "OPEN"}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--dest", base)
	}
	args = append(args, h.repoArgs()...)
	args = append(args, "-o", "json")
	out, err := h.cmd(ctx, "twg", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests query: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		// Nonempty output with no JSON delimiter is not a legitimate "no open
		// PRs" response (an empty list still prints "[]", which has a
		// delimiter) - it must surface as an error rather than be read as
		// absence, which would otherwise cause the PR step to attempt a
		// duplicate create or report a misleading creation failure.
		return nil, fmt.Errorf("twg bb pull-requests query: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var items []bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests query: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if len(items) == 0 {
		return nil, nil
	}
	for i := range items {
		if items[i].ID <= 0 {
			return nil, fmt.Errorf("twg bb pull-requests query: entry %d missing positive id", i)
		}
	}
	return items[0].toPR(h.repo), nil
}

// runWithDescription runs a twg bb pull-requests command whose description is
// supplied out-of-band through a temp file referenced by
// `--description-file <path>`, and returns the command's combined output.
// buildArgs receives the file path and returns the full twg argv, placing
// that path where --description-file's value belongs. A temp file (rather
// than passing the body inline as --description) avoids OS argument-length
// limits on large PR bodies and matches twg's own documented convention for
// non-trivial descriptions.
func (h *Host) runWithDescription(ctx context.Context, body string, buildArgs func(descPath string) []string) ([]byte, error) {
	f, err := os.CreateTemp("", "nm-bb-pr-desc-*.md")
	if err != nil {
		return nil, fmt.Errorf("create PR description temp file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return nil, fmt.Errorf("write PR description temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close PR description temp file: %w", err)
	}
	out, err := h.cmd(ctx, "twg", buildArgs(path)...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	out, err := h.runWithDescription(ctx, content.Body, func(descPath string) []string {
		args := []string{"bb", "pull-requests", "create",
			"--title", content.Title,
			"--source", branch,
			"--description-file", descPath,
		}
		if strings.TrimSpace(base) != "" {
			args = append(args, "--dest", base)
		}
		if h.draft {
			args = append(args, "--draft")
		}
		args = append(args, h.repoArgs()...)
		return append(args, "-o", "json")
	})
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests create: %w", err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("twg bb pull-requests create: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &pr); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests create: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if pr.ID <= 0 {
		return nil, fmt.Errorf("twg bb pull-requests create: missing positive id: %s", strings.TrimSpace(string(out)))
	}
	return pr.toPR(h.repo), nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	if pr == nil || strings.TrimSpace(pr.Number) == "" {
		return nil, errors.New("twg bb pull-requests update: missing pull request number")
	}
	out, err := h.runWithDescription(ctx, content.Body, func(descPath string) []string {
		args := []string{"bb", "pull-requests", "update",
			"--pull-request", pr.Number,
			"--description-file", descPath,
		}
		if strings.TrimSpace(content.Title) != "" {
			args = append(args, "--title", content.Title)
		}
		args = append(args, h.repoArgs()...)
		return append(args, "-o", "json")
	})
	if err != nil {
		return nil, fmt.Errorf("twg bb pull-requests update: %w", err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("twg bb pull-requests update: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var updated bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &updated); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests update: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return updated.toPR(h.repo), nil
}

func (h *Host) getPR(ctx context.Context, number string, extraArgs ...string) (bitbucketPullRequest, []byte, error) {
	args := append([]string{"bb", "pull-requests", "get", number}, extraArgs...)
	args = append(args, h.repoArgs()...)
	args = append(args, "-o", "json")
	out, err := h.cmd(ctx, "twg", args...).CombinedOutput()
	if err != nil {
		return bitbucketPullRequest{}, nil, fmt.Errorf("twg bb pull-requests get: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return bitbucketPullRequest{}, nil, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var pr bitbucketPullRequest
	if err := json.Unmarshal(trimmed, &pr); err != nil {
		return bitbucketPullRequest{}, nil, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return pr, trimmed, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	view, _, err := h.getPR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	return normalizePRState(view.State), nil
}

// pullRequestStatuses reads the PR's hydrated build/CI statuses via
// `twg bb pull-requests get --statuses`.
func (h *Host) pullRequestStatuses(ctx context.Context, prNumber string) ([]CommitStatus, error) {
	_, trimmed, err := h.getPR(ctx, prNumber, "--statuses")
	if err != nil {
		return nil, err
	}
	var view struct {
		Statuses []CommitStatus `json:"_statuses"`
	}
	if err := json.Unmarshal(trimmed, &view); err != nil {
		return nil, fmt.Errorf("twg bb pull-requests get --statuses: invalid JSON output: %s", strings.TrimSpace(string(trimmed)))
	}
	return view.Statuses, nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	statuses, err := h.pullRequestStatuses(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	statuses = LatestStatuses(statuses)
	checks := make([]scm.Check, 0, len(statuses))
	for _, status := range statuses {
		checks = append(checks, scm.Check{
			Name:        statusName(status),
			ProviderID:  statusProviderID(status),
			Bucket:      statusBucket(status.State),
			ExecutionID: pipelineBuildNumberFromStatusURL(status.URL),
		})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(_ context.Context, _ *scm.PR) (scm.MergeableState, error) {
	return "", scm.ErrUnsupported
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

func (h *Host) FetchFailedCheckTargetLogs(ctx context.Context, pr *scm.PR, _ string, _ string, targets []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	statuses, err := h.pullRequestStatuses(ctx, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("resolve selected Bitbucket checks: %w", err)
	}
	targetBuildNumbers := make([]map[string]struct{}, len(targets))
	for i, target := range targets {
		resolved, err := failedPipelineBuildNumberTargets(statuses, []scm.CheckTarget{target})
		if err == nil {
			targetBuildNumbers[i] = resolved
		}
	}
	cache := map[string]scm.FailedCheckLog{}
	results := make([]scm.FailedCheckLog, 0, len(targets))
	for i, target := range targets {
		result := scm.FailedCheckLog{Target: target}
		if len(targetBuildNumbers[i]) == 0 {
			result.Err = fmt.Errorf("selected Bitbucket check %q was not found", target.Identity())
			results = append(results, result)
			continue
		}
		var outputs []string
		var logErrors []error
		for buildNumber := range targetBuildNumbers[i] {
			cached, ok := cache[buildNumber]
			if !ok {
				cached.Output, cached.Err = h.fetchPipelineLog(ctx, buildNumber)
				cache[buildNumber] = cached
			}
			if cached.Output != "" {
				outputs = append(outputs, cached.Output)
			}
			if cached.Err != nil {
				logErrors = append(logErrors, cached.Err)
			}
		}
		result.Output = strings.Join(outputs, "\n\n")
		result.Err = errors.Join(logErrors...)
		results = append(results, result)
	}
	return results, nil
}

// fetchPipelineLog returns the failed-step log text for one pipeline build.
// twg's `pipeline get --logs --failed-steps` already hydrates only the
// failed/error/stopped steps' logs, so unlike the old HTTP client this needs
// no separate step-listing call: the human-readable text output is the log
// payload itself, sidestepping any dependency on undocumented JSON step
// shapes for a value that is opaque text either way.
func (h *Host) fetchPipelineLog(ctx context.Context, buildNumber string) (string, error) {
	args := []string{"bb", "pipeline", "get", "--pipeline", buildNumber, "--logs", "--failed-steps", "--lines", "0"}
	args = append(args, h.repoArgs()...)
	out, err := h.cmd(ctx, "twg", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("twg bb pipeline get --logs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func prURL(repo RepoRef, prID int, rawURL string) string {
	if url := strings.TrimSpace(rawURL); url != "" {
		return url
	}
	if prID <= 0 || strings.TrimSpace(repo.Workspace) == "" || strings.TrimSpace(repo.RepoSlug) == "" {
		return ""
	}
	return fmt.Sprintf("https://bitbucket.org/%s/%s/pull-requests/%d", repo.Workspace, repo.RepoSlug, prID)
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OPEN":
		return scm.PRStateOpen
	case "MERGED":
		return scm.PRStateMerged
	case "DECLINED", "CLOSED", "SUPERSEDED":
		return scm.PRStateClosed
	default:
		return scm.PRState(raw)
	}
}

// LatestStatuses keeps only the newest status per unique key/name.
func LatestStatuses(statuses []CommitStatus) []CommitStatus {
	latest := make([]CommitStatus, 0, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		id := strings.TrimSpace(status.Key)
		if id == "" {
			id = statusName(status)
		}
		if id == "" {
			latest = append(latest, status)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		latest = append(latest, status)
	}
	return latest
}

func statusName(status CommitStatus) string {
	name := strings.TrimSpace(status.Name)
	if name != "" {
		return name
	}
	return strings.TrimSpace(status.Key)
}

func statusProviderID(status CommitStatus) string {
	if key := strings.TrimSpace(status.Key); key != "" {
		return "bitbucket-status:" + key
	}
	if buildNumber := pipelineBuildNumberFromStatusURL(status.URL); buildNumber != "" {
		return "bitbucket-pipeline-build:" + buildNumber
	}
	return ""
}

func statusBucket(state string) scm.CheckBucket {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESSFUL", "SUCCESS":
		return scm.CheckBucketPass
	case "FAILED", "FAILURE", "ERROR":
		return scm.CheckBucketFail
	case "STOPPED":
		return scm.CheckBucketCancel
	case "INPROGRESS", "IN_PROGRESS", "PENDING":
		return scm.CheckBucketPending
	default:
		return ""
	}
}

func pipelineBuildNumberFromStatusURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	fragments := []string{parsed.Fragment, parsed.Path}
	for _, fragment := range fragments {
		idx := strings.LastIndex(fragment, "/results/")
		if idx < 0 {
			continue
		}
		buildNumber := fragment[idx+len("/results/"):]
		buildNumber = strings.TrimSpace(strings.SplitN(buildNumber, "?", 2)[0])
		buildNumber = strings.TrimSpace(strings.SplitN(buildNumber, "/", 2)[0])
		buildNumber = strings.Trim(buildNumber, "{}")
		if number, err := strconv.Atoi(buildNumber); err == nil && number > 0 {
			return strconv.Itoa(number)
		}
		return ""
	}
	return ""
}

func failedPipelineBuildNumberTargets(statuses []CommitStatus, selected []scm.CheckTarget) (map[string]struct{}, error) {
	latest := LatestStatuses(statuses)
	targets := make(map[string]struct{}, len(selected))
	var resolveErrors []error
	for _, target := range selected {
		matched := false
		for _, status := range latest {
			if statusBucket(status.State) != scm.CheckBucketFail {
				continue
			}
			if target.ProviderID != "" {
				if statusProviderID(status) != target.ProviderID {
					continue
				}
			} else if statusName(status) != strings.TrimSpace(target.Name) {
				continue
			}
			matched = true
			if buildNumber := pipelineBuildNumberFromStatusURL(status.URL); buildNumber != "" {
				targets[buildNumber] = struct{}{}
			} else {
				resolveErrors = append(resolveErrors, fmt.Errorf("selected Bitbucket check %q has no pipeline identity", statusName(status)))
			}
		}
		if !matched {
			identity := target.ProviderID
			if identity == "" {
				identity = target.Name
			}
			resolveErrors = append(resolveErrors, fmt.Errorf("selected Bitbucket check %q was not found", identity))
		}
	}
	if err := errors.Join(resolveErrors...); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("no selected Bitbucket checks resolved to pipelines")
	}
	return targets, nil
}

// bytesTrimToJSON skips any banner text before the first '{' or '[', mirroring
// the same defensive scan the Gitea CLI-shelled Host uses: a version-check
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

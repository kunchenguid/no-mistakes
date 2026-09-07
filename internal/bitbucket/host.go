package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Host implements scm.Host for Bitbucket using the REST API client.
type Host struct {
	client *Client
	repo   RepoRef
	draft  bool // open created PRs as drafts ("draft": true in the create body)
}

// NewHost builds a Host from an API client and a parsed repository reference.
// When draft is true, created PRs are opened as drafts.
func NewHost(client *Client, repo RepoRef, draft bool) *Host {
	return &Host{client: client, repo: repo, draft: draft}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderBitbucket }

// Capabilities reports Bitbucket's feature matrix. Bitbucket's REST API
// does not expose a reliable merge-conflict probe, so MergeableState is off.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: false, FailedCheckLogs: true}
}

func (h *Host) Available(_ context.Context) error {
	if h.client == nil {
		return errors.New("bitbucket client is not configured")
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	pr, err := h.client.FindOpenPRBySourceBranch(ctx, h.repo, branch, base)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, nil
	}
	return h.toPR(pr), nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	pr, err := h.client.CreatePR(ctx, h.repo, branch, base, content.Title, content.Body, h.draft)
	if err != nil {
		return nil, err
	}
	return h.toPR(pr), nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return nil, fmt.Errorf("invalid Bitbucket PR number %q: %w", pr.Number, err)
	}
	updated, err := h.client.UpdatePR(ctx, h.repo, id, content.Title, content.Body)
	if err != nil {
		return nil, err
	}
	return h.toPR(updated), nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return "", err
	}
	got, err := h.client.GetPR(ctx, h.repo, id)
	if err != nil {
		return "", err
	}
	if got == nil {
		return "", nil
	}
	return normalizePRState(got.State), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return nil, err
	}
	statuses, err := h.client.ListPRStatuses(ctx, h.repo, id)
	if err != nil {
		return nil, err
	}
	statuses = LatestStatuses(statuses)
	checks := make([]scm.Check, 0, len(statuses))
	for _, status := range statuses {
		checks = append(checks, scm.Check{
			Name:       statusName(status),
			ProviderID: statusProviderID(status),
			Bucket:     statusBucket(status.State),
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

func (h *Host) FetchFailedCheckTargetLogs(ctx context.Context, pr *scm.PR, _ string, headSHA string, selected []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	if h.client == nil {
		return nil, errors.New("Bitbucket client is not configured")
	}
	if len(selected) == 0 {
		return nil, nil
	}
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return nil, err
	}
	commitSHA := strings.TrimSpace(headSHA)
	if got, prErr := h.client.GetPR(ctx, h.repo, id); prErr == nil && got != nil && strings.TrimSpace(got.SourceCommitHash) != "" {
		commitSHA = strings.TrimSpace(got.SourceCommitHash)
	}
	statuses, err := h.client.ListPRStatuses(ctx, h.repo, id)
	if err != nil {
		return nil, fmt.Errorf("resolve selected Bitbucket checks: %w", err)
	}
	targetBuildNumbers := make([]map[string]struct{}, len(selected))
	for i, target := range selected {
		resolved, err := failedPipelineBuildNumberTargets(statuses, []scm.CheckTarget{target})
		if err == nil {
			targetBuildNumbers[i] = resolved
		}
	}
	if strings.TrimSpace(commitSHA) == "" {
		return nil, errors.New("resolve selected Bitbucket checks: pull request head commit is empty")
	}
	pipelines, err := h.client.ListPipelinesByCommit(ctx, h.repo, commitSHA)
	if err != nil {
		return nil, fmt.Errorf("list selected Bitbucket pipelines: %w", err)
	}
	pipelineByBuild := make(map[string]Pipeline, len(pipelines))
	for _, pipelineRun := range pipelines {
		if pipelineRun.BuildNumber > 0 {
			pipelineByBuild[strconv.Itoa(pipelineRun.BuildNumber)] = pipelineRun
		}
	}
	cache := map[string]scm.FailedCheckLog{}
	results := make([]scm.FailedCheckLog, 0, len(selected))
	for i, target := range selected {
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
				pipelineRun, found := pipelineByBuild[buildNumber]
				if !found {
					cached.Err = fmt.Errorf("selected Bitbucket pipeline build %s was not found for commit %s", buildNumber, commitSHA)
				} else {
					cached.Output, cached.Err = h.fetchPipelineLogs(ctx, pipelineRun)
				}
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

func (h *Host) fetchPipelineLogs(ctx context.Context, pipelineRun Pipeline) (string, error) {
	steps, err := h.client.ListPipelineSteps(ctx, h.repo, pipelineRun.UUID)
	if err != nil {
		return "", fmt.Errorf("list Bitbucket pipeline %s steps: %w", pipelineRun.UUID, err)
	}
	var logs []string
	var logErrors []error
	for _, step := range steps {
		if !strings.EqualFold(step.State.Result.Name, "FAILED") {
			continue
		}
		logOutput, err := h.client.GetStepLog(ctx, h.repo, pipelineRun.UUID, step.UUID)
		if err != nil {
			logErrors = append(logErrors, fmt.Errorf("fetch Bitbucket pipeline %s step %s log: %w", pipelineRun.UUID, step.UUID, err))
			continue
		}
		if log := strings.TrimSpace(logOutput); log != "" {
			logs = append(logs, log)
		}
	}
	return strings.Join(logs, "\n\n"), errors.Join(logErrors...)
}

func (h *Host) toPR(pr *PullRequest) *scm.PR {
	if pr == nil {
		return nil
	}
	return &scm.PR{
		Number: strconv.Itoa(pr.ID),
		URL:    prURL(h.repo, pr.ID, pr.URL),
	}
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
// Exported because legacy step code still calls it by name during the migration.
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

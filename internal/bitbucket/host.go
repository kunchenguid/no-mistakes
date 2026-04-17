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
}

// NewHost builds a Host from an API client and a parsed repository reference.
func NewHost(client *Client, repo RepoRef) *Host {
	return &Host{client: client, repo: repo}
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
	pr, err := h.client.CreatePR(ctx, h.repo, branch, base, content.Title, content.Body)
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
			Name:   statusName(status),
			Bucket: statusBucket(status.State),
		})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(_ context.Context, _ *scm.PR) (scm.MergeableState, error) {
	return "", scm.ErrUnsupported
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, headSHA string, failingNames []string) (string, error) {
	if h.client == nil {
		return "", nil
	}
	id, err := strconv.Atoi(pr.Number)
	if err != nil {
		return "", err
	}
	commitSHA := strings.TrimSpace(headSHA)
	var targets map[string]struct{}
	if got, prErr := h.client.GetPR(ctx, h.repo, id); prErr == nil && got != nil && strings.TrimSpace(got.SourceCommitHash) != "" {
		commitSHA = strings.TrimSpace(got.SourceCommitHash)
	}
	if statuses, statusErr := h.client.ListPRStatuses(ctx, h.repo, id); statusErr == nil {
		targets = failedPipelineUUIDs(statuses, failingNames)
	}
	if strings.TrimSpace(commitSHA) == "" {
		return "", nil
	}
	pipelines, err := h.client.ListPipelinesByCommit(ctx, h.repo, commitSHA)
	if err != nil {
		return "", nil
	}
	for _, pipelineRun := range pipelines {
		if len(targets) > 0 {
			if _, ok := targets[normalizePipelineUUID(pipelineRun.UUID)]; !ok {
				continue
			}
		}
		steps, err := h.client.ListPipelineSteps(ctx, h.repo, pipelineRun.UUID)
		if err != nil {
			continue
		}
		for _, step := range steps {
			if !strings.EqualFold(step.State.Result.Name, "FAILED") {
				continue
			}
			logOutput, err := h.client.GetStepLog(ctx, h.repo, pipelineRun.UUID, step.UUID)
			if err != nil || strings.TrimSpace(logOutput) == "" {
				continue
			}
			return strings.TrimSpace(logOutput), nil
		}
	}
	return "", nil
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

func normalizePipelineUUID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.Trim(trimmed, "{}")
	if trimmed == "" {
		return ""
	}
	return strings.ToLower(trimmed)
}

func pipelineUUIDFromStatusURL(raw string) string {
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
		uuid := fragment[idx+len("/results/"):]
		uuid = strings.TrimSpace(strings.SplitN(uuid, "?", 2)[0])
		uuid = strings.TrimSpace(strings.SplitN(uuid, "/", 2)[0])
		return normalizePipelineUUID(uuid)
	}
	return ""
}

func failedPipelineUUIDs(statuses []CommitStatus, failingNames []string) map[string]struct{} {
	if len(failingNames) == 0 {
		return nil
	}
	failing := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			failing[trimmed] = struct{}{}
		}
	}
	if len(failing) == 0 {
		return nil
	}
	targets := map[string]struct{}{}
	for _, status := range LatestStatuses(statuses) {
		if _, ok := failing[statusName(status)]; !ok {
			continue
		}
		uuid := pipelineUUIDFromStatusURL(status.URL)
		if uuid != "" {
			targets[uuid] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	return targets
}

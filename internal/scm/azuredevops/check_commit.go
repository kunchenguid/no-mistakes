package azuredevops

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A policy evaluation's context is opaque. A build ID is only a lookup key;
// exact-head evidence comes from the native build and PR merge/source tuple.
func (h *Host) checkSourceCommit(ctx context.Context, pr *scm.PR, e policyEval) (string, error) {
	switch strings.ToLower(strings.TrimSpace(e.Configuration.Type.DisplayName)) {
	case "build":
		return h.buildSourceCommit(ctx, pr, e)
	case "status":
		return h.statusSourceCommit(ctx, pr, e)
	}
	return "", nil
}

func (h *Host) buildSourceCommit(ctx context.Context, pr *scm.PR, e policyEval) (string, error) {
	id, ok := e.Context["buildId"].(float64)
	if !ok || id <= 0 || id != float64(int(id)) || e.Configuration.Settings.BuildDefinitionID <= 0 {
		return "", nil
	}
	args := []string{"pipelines", "runs", "show", "--id", strconv.Itoa(int(id)), "--organization", h.org, "--project", h.project, "--output", "json"}
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return "", fmt.Errorf("read Azure policy build: %w", err)
	}
	var build struct {
		ID         int `json:"id"`
		Definition struct {
			ID int `json:"id"`
		} `json:"definition"`
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
		Repository struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"repository"`
		Status        string `json:"status"`
		Result        string `json:"result"`
		SourceBranch  string `json:"sourceBranch"`
		SourceVersion string `json:"sourceVersion"`
	}
	if err := json.Unmarshal(out, &build); err != nil {
		return "", fmt.Errorf("parse Azure policy build: %w", err)
	}
	if build.ID != int(id) || build.Definition.ID != e.Configuration.Settings.BuildDefinitionID || !strings.EqualFold(build.Project.Name, h.project) || build.Repository.Type != "TfsGit" || build.SourceBranch != "refs/pull/"+h.prID(pr)+"/merge" || !azFullSHA(build.SourceVersion) {
		return "", nil
	}
	if azStatusBucket(e.Status) == scm.CheckBucketPass && (build.Status != "completed" || build.Result != "succeeded") {
		return "", nil
	}
	raw, err := h.showPR(ctx, pr)
	if err != nil {
		return "", err
	}
	facts, err := h.factsFromPR(*raw)
	if err != nil {
		return "", err
	}
	if facts.PR.Number != h.prID(pr) || raw.Repository.ID == "" || raw.Repository.ID != build.Repository.ID || raw.MergeStatus != "succeeded" || raw.LastMergeCommit.CommitID != build.SourceVersion {
		return "", nil
	}
	facts, err = h.bindLiveHead(ctx, facts, map[string]string{})
	if err != nil {
		return "", err
	}
	return facts.HeadSHA, nil
}

func (h *Host) invokePRList(ctx context.Context, pr *scm.PR, resource string, target any) error {
	args := []string{"devops", "invoke", "--area", "git", "--resource", resource, "--route-parameters", "project=" + h.project, "repositoryId=" + h.repo, "pullRequestId=" + h.prID(pr), "--organization", h.org, "--api-version", "7.1", "--output", "json"}
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return fmt.Errorf("read Azure %s: %w", resource, err)
	}
	if err := json.Unmarshal(out, target); err != nil {
		return fmt.Errorf("parse Azure %s: %w", resource, err)
	}
	return nil
}

type azPRStatus struct {
	ID          int    `json:"id"`
	IterationID int    `json:"iterationId"`
	State       string `json:"state"`
	UpdatedDate string `json:"updatedDate"`
	Context     struct {
		Name  string `json:"name"`
		Genre string `json:"genre"`
	} `json:"context"`
	CreatedBy struct {
		ID string `json:"id"`
	} `json:"createdBy"`
}

// PR-level statuses are deliberately code-independent. Only an iteration
// status can supply a source revision, and a newer ambiguous status earns none.
func (h *Host) statusSourceCommit(ctx context.Context, pr *scm.PR, e policyEval) (string, error) {
	settings := e.Configuration.Settings
	if settings.StatusName == "" {
		return "", nil
	}
	var statuses struct {
		Count int          `json:"count"`
		Value []azPRStatus `json:"value"`
	}
	if err := h.invokePRList(ctx, pr, "pullRequestStatuses", &statuses); err != nil {
		return "", err
	}
	if statuses.Value == nil || statuses.Count != len(statuses.Value) {
		return "", fmt.Errorf("Azure PR statuses response is incomplete")
	}
	var latest *azPRStatus
	for i := range statuses.Value {
		status := &statuses.Value[i]
		if status.Context.Name != settings.StatusName || status.Context.Genre != settings.StatusGenre || (settings.AuthorID != "" && status.CreatedBy.ID != settings.AuthorID) {
			continue
		}
		if latest == nil {
			latest = status
			continue
		}
		next, prior := parseAzTime(status.UpdatedDate), parseAzTime(latest.UpdatedDate)
		if next.IsZero() || prior.IsZero() || next.Equal(prior) {
			return "", nil
		}
		if next.After(prior) {
			latest = status
		}
	}
	if latest == nil || latest.ID <= 0 || latest.IterationID <= 0 {
		return "", nil
	}
	bucket := ""
	switch latest.State {
	case "succeeded":
		bucket = "pass"
	case "failed", "error":
		bucket = "fail"
	case "pending":
		bucket = "pending"
	}
	if bucket == "" || bucket != string(azStatusBucket(e.Status)) {
		return "", nil
	}
	var iterations struct {
		Count int `json:"count"`
		Value []struct {
			ID              int `json:"id"`
			SourceRefCommit struct {
				CommitID string `json:"commitId"`
			} `json:"sourceRefCommit"`
		} `json:"value"`
	}
	if err := h.invokePRList(ctx, pr, "pullRequestIterations", &iterations); err != nil {
		return "", err
	}
	if iterations.Value == nil || iterations.Count != len(iterations.Value) {
		return "", fmt.Errorf("Azure PR iterations response is incomplete")
	}
	head := ""
	for _, iteration := range iterations.Value {
		if iteration.ID == latest.IterationID {
			if head != "" || !azFullSHA(iteration.SourceRefCommit.CommitID) {
				return "", nil
			}
			head = iteration.SourceRefCommit.CommitID
		}
	}
	return head, nil
}

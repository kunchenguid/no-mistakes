package steps

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// getPRState returns the PR state (OPEN, MERGED, CLOSED).
func (s *CIStep) getPRState(sctx *pipeline.StepContext, provider scm.Provider, bitbucketClient *bitbucket.Client, bitbucketRepo bitbucket.RepoRef, prNumber string) (string, error) {
	if provider == scm.ProviderBitbucket {
		prID, err := strconv.Atoi(prNumber)
		if err != nil {
			return "", err
		}
		pr, err := bitbucketClient.GetPR(sctx.Ctx, bitbucketRepo, prID)
		if err != nil {
			return "", err
		}
		return pr.State, nil
	}
	cmd := stepCmd(sctx, "gh", "pr", "view", prNumber, "--json", "state", "--jq", ".state")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// getMergeableState returns the PR mergeable state (MERGEABLE, CONFLICTING, UNKNOWN).
func (s *CIStep) getMergeableState(sctx *pipeline.StepContext, prNumber string) (string, error) {
	cmd := stepCmd(sctx, "gh", "pr", "view", prNumber, "--json", "mergeable", "--jq", ".mergeable")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// getCIChecks fetches CI check results for a PR.
func (s *CIStep) getCIChecks(sctx *pipeline.StepContext, provider scm.Provider, bitbucketClient *bitbucket.Client, bitbucketRepo bitbucket.RepoRef, prNumber string) ([]ciCheck, error) {
	if provider == scm.ProviderBitbucket {
		prID, err := strconv.Atoi(prNumber)
		if err != nil {
			return nil, err
		}
		statuses, err := bitbucketClient.ListPRStatuses(sctx.Ctx, bitbucketRepo, prID)
		if err != nil {
			return nil, err
		}
		statuses = latestBitbucketStatuses(statuses)
		checks := make([]ciCheck, 0, len(statuses))
		for _, status := range statuses {
			checks = append(checks, ciCheck{
				Name:   bitbucketStatusName(status),
				State:  status.State,
				Bucket: bitbucketStatusBucket(status.State),
			})
		}
		return checks, nil
	}
	cmd := stepCmd(sctx, "gh", "pr", "checks", prNumber, "--json", "name,state,bucket")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no checks reported") {
			return nil, nil
		}
		return nil, fmt.Errorf("gh pr checks: %w", err)
	}
	var checks []ciCheck
	if err := json.Unmarshal(out, &checks); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	return checks, nil
}

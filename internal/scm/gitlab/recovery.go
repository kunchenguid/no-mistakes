package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

type recoveryMergeRequest struct {
	IID          int    `json:"iid"`
	SourceBranch string `json:"source_branch"`
	SHA          string `json:"sha"`
	DiffRefs     struct {
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
}

func (h *Host) VerifyUnpublishedHistory(ctx context.Context, branch, submitted, preserved string) error {
	if err := h.Available(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(h.projectPath) == "" {
		return errors.New("GitLab project identity is unavailable")
	}
	project := url.PathEscape(h.projectPath)
	var mergeRequests []recoveryMergeRequest
	if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests?state=all&source_branch=%s&per_page=100", project, url.QueryEscape(branch)), &mergeRequests); err != nil {
		return fmt.Errorf("inspect GitLab merge-request history: %w", err)
	}
	for _, mergeRequest := range mergeRequests {
		if mergeRequest.SourceBranch != branch {
			continue
		}
		head := mergeRequest.SHA
		if head == "" {
			head = mergeRequest.DiffRefs.HeadSHA
		}
		if head == "" || head != submitted {
			return fmt.Errorf("GitLab merge request %d has a changed head", mergeRequest.IID)
		}
		var versions []struct {
			HeadCommitSHA string `json:"head_commit_sha"`
			CreatedAt     string `json:"created_at"`
		}
		if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests/%d/versions?per_page=100", project, mergeRequest.IID), &versions); err != nil {
			return fmt.Errorf("inspect GitLab merge-request %d history: %w", mergeRequest.IID, err)
		}
		for _, version := range versions {
			if version.HeadCommitSHA == preserved {
				return fmt.Errorf("GitLab merge request %d history contains the preserved unpublished head", mergeRequest.IID)
			}
		}
	}
	return nil
}

func (h *Host) apiPages(ctx context.Context, endpoint string, dst interface{}) error {
	args := []string{"api", "--paginate"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	cmd := h.cmd(ctx, "glab", args...)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	var merged []json.RawMessage
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		} else {
			merged = append(merged, raw)
		}
	}
	if len(merged) == 0 {
		return errors.New("GitLab API returned no JSON")
	}
	var combined []byte
	if len(merged) == 1 {
		combined = merged[0]
	} else {
		var values []json.RawMessage
		for _, raw := range merged {
			var page []json.RawMessage
			if err := json.Unmarshal(raw, &page); err != nil {
				return err
			}
			values = append(values, page...)
		}
		var err error
		combined, err = json.Marshal(values)
		if err != nil {
			return err
		}
	}
	return json.Unmarshal(combined, dst)
}

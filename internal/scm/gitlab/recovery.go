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
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type recoveryMergeRequest struct {
	IID          int    `json:"iid"`
	SourceBranch string `json:"source_branch"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	SHA          string `json:"sha"`
	DiffRefs     struct {
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
}

func (h *Host) VerifyUnpublishedHistory(ctx context.Context, branch, submitted, preserved string, since, until int64, targetIdentity string) error {
	if err := h.Available(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(h.projectPath) == "" {
		return errors.New("GitLab project identity is unavailable")
	}
	targetNumber := ""
	if strings.TrimSpace(targetIdentity) != "" {
		var err error
		targetNumber, err = scm.ExtractPRNumber(targetIdentity)
		if err != nil || (h.host != "" && scm.ExtractHost(targetIdentity) != "" && !strings.EqualFold(scm.ExtractHost(targetIdentity), h.host)) || scmProjectPath(targetIdentity) != h.projectPath {
			return errors.New("GitLab submission-time merge-request identity is unavailable or mismatched")
		}
	}
	project := url.PathEscape(h.projectPath)
	var mergeRequests []recoveryMergeRequest
	if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests?state=all&per_page=100", project), &mergeRequests); err != nil {
		return fmt.Errorf("inspect GitLab merge-request history: %w", err)
	}
	matched := false
	for _, mergeRequest := range mergeRequests {
		inWindow, err := recoveryRecordInWindow(mergeRequest.CreatedAt, mergeRequest.UpdatedAt, since, until)
		if err != nil {
			return fmt.Errorf("GitLab merge request %d has incomplete historical timestamps: %w", mergeRequest.IID, err)
		}
		if !inWindow {
			continue
		}
		if targetNumber != "" && fmt.Sprint(mergeRequest.IID) != targetNumber {
			continue
		}
		if targetNumber == "" && mergeRequest.SourceBranch != branch {
			return fmt.Errorf("GitLab merge request %d has no complete submission-time target identity", mergeRequest.IID)
		}
		matched = true
		head := mergeRequest.SHA
		if head == "" {
			head = mergeRequest.DiffRefs.HeadSHA
		}
		if head == "" || head != submitted {
			return fmt.Errorf("GitLab merge request %d has a changed head", mergeRequest.IID)
		}
		var versions []json.RawMessage
		if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests/%d/versions?per_page=100", project, mergeRequest.IID), &versions); err != nil {
			return fmt.Errorf("inspect GitLab merge-request %d history: %w", mergeRequest.IID, err)
		}
		for _, version := range versions {
			if recoveryJSONContainsSHA(version, preserved) {
				return fmt.Errorf("GitLab merge request %d history contains the preserved unpublished head", mergeRequest.IID)
			}
		}
		var events []json.RawMessage
		if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests/%d/resource_state_events?per_page=100", project, mergeRequest.IID), &events); err != nil {
			return fmt.Errorf("inspect GitLab merge-request %d event history: %w", mergeRequest.IID, err)
		}
		for _, event := range events {
			if recoveryJSONContainsSHA(event, preserved) {
				return fmt.Errorf("GitLab merge request %d history contains the preserved unpublished head", mergeRequest.IID)
			}
		}
	}
	if !matched && targetNumber != "" {
		return fmt.Errorf("GitLab merge request %s was not found in the submission interval", targetNumber)
	}
	return nil
}

func scmProjectPath(identity string) string {
	trimmed := strings.TrimSpace(identity)
	if marker := strings.Index(trimmed, "/-/merge_requests/"); marker >= 0 {
		path := strings.TrimPrefix(trimmed[:marker], "https://")
		path = strings.TrimPrefix(path, "http://")
		if slash := strings.Index(path, "/"); slash >= 0 {
			return strings.Trim(path[slash+1:], "/")
		}
	}
	return ""
}

func recoveryRecordInWindow(created, updated string, since, until int64) (bool, error) {
	if since == 0 && until == 0 {
		return true, nil
	}
	if since <= 0 || until < since || strings.TrimSpace(created) == "" || strings.TrimSpace(updated) == "" {
		return false, errors.New("missing or invalid run interval")
	}
	createdAt, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return false, err
	}
	updatedAt, err := time.Parse(time.RFC3339, updated)
	if err != nil {
		return false, err
	}
	return !updatedAt.Before(time.Unix(since, 0)) && !createdAt.After(time.Unix(until, 0)), nil
}

func recoveryJSONContainsSHA(raw json.RawMessage, preserved string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return recoveryValueContainsSHA(value, preserved)
}

func recoveryValueContainsSHA(value any, preserved string) bool {
	switch typed := value.(type) {
	case string:
		return typed == preserved
	case []any:
		for _, item := range typed {
			if recoveryValueContainsSHA(item, preserved) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if recoveryValueContainsSHA(item, preserved) {
				return true
			}
		}
	}
	return false
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

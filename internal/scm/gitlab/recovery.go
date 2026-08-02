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
		var events []json.RawMessage
		if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests/%d/resource_state_events?per_page=100", project, mergeRequest.IID), &events); err != nil {
			return fmt.Errorf("inspect GitLab merge-request %d event history: %w", mergeRequest.IID, err)
		}
		if targetNumber == "" && mergeRequest.SourceBranch != branch {
			related, explicit := recoveryBranchRelation(events, branch)
			if !related {
				if !explicit {
					return fmt.Errorf("GitLab merge request %d has incomplete submission-time target lineage", mergeRequest.IID)
				}
				continue
			}
		}
		matched = true
		head := mergeRequest.SHA
		if head == "" {
			head = mergeRequest.DiffRefs.HeadSHA
		}
		if targetNumber != "" && (head == "" || head != submitted) {
			return fmt.Errorf("GitLab merge request %d has a changed head", mergeRequest.IID)
		}
		if head == preserved {
			return fmt.Errorf("GitLab merge request %d history contains the preserved unpublished head", mergeRequest.IID)
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

func (h *Host) VerifyUnpublishedRefHistory(ctx context.Context, branch, submitted, preserved string, since, until int64) error {
	if err := h.Available(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(h.projectPath) == "" {
		return errors.New("GitLab project identity is unavailable")
	}
	project := url.PathEscape(h.projectPath)
	var events []json.RawMessage
	if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/events?per_page=100", project), &events); err != nil {
		return fmt.Errorf("inspect GitLab ref history: %w", err)
	}
	for _, event := range events {
		inWindow, err := recoveryEventInWindow(event, since, until)
		if err != nil {
			return fmt.Errorf("GitLab ref history has incomplete timestamps: %w", err)
		}
		if !inWindow {
			continue
		}
		related, _ := recoveryBranchRelation([]json.RawMessage{event}, branch)
		if related {
			if recoveryRefEventContainsSHA(event, preserved) {
				return fmt.Errorf("GitLab branch %s history contains the preserved unpublished head", branch)
			}
			if recoveryRefEventChanged(event, submitted) {
				return fmt.Errorf("GitLab branch %s history contains a head other than the submitted head", branch)
			}
		}
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

func recoveryEventInWindow(raw json.RawMessage, since, until int64) (bool, error) {
	if since == 0 && until == 0 {
		return true, nil
	}
	if since <= 0 || until < since {
		return false, errors.New("missing or invalid run interval")
	}
	var event struct {
		CreatedAt string `json:"created_at"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return false, err
	}
	stamp := strings.TrimSpace(event.CreatedAt)
	if stamp == "" {
		stamp = strings.TrimSpace(event.Timestamp)
	}
	if stamp == "" {
		return false, errors.New("missing event timestamp")
	}
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return false, err
	}
	return !when.Before(time.Unix(since, 0)) && !when.After(time.Unix(until, 0)), nil
}

func recoveryBranchRelation(events []json.RawMessage, branch string) (bool, bool) {
	wanted := normalizeRecoveryBranch(branch)
	matched, explicit := false, false
	for _, event := range events {
		var value any
		if json.Unmarshal(event, &value) != nil {
			continue
		}
		eventMatched, eventExplicit := recoveryValueBranchRelation(value, wanted)
		matched = matched || eventMatched
		explicit = explicit || eventExplicit
	}
	return matched, explicit
}

func recoveryValueBranchRelation(value any, wanted string) (bool, bool) {
	switch typed := value.(type) {
	case []any:
		matched, explicit := false, false
		for _, item := range typed {
			itemMatched, itemExplicit := recoveryValueBranchRelation(item, wanted)
			matched = matched || itemMatched
			explicit = explicit || itemExplicit
		}
		return matched, explicit
	case map[string]any:
		matched, explicit := false, false
		for key, item := range typed {
			if branchScopeKey(key) {
				if candidate, ok := item.(string); ok && normalizeRecoveryBranch(candidate) != "" {
					explicit = true
					matched = matched || normalizeRecoveryBranch(candidate) == wanted
				}
			}
			itemMatched, itemExplicit := recoveryValueBranchRelation(item, wanted)
			matched = matched || itemMatched
			explicit = explicit || itemExplicit
		}
		return matched, explicit
	}
	return false, false
}

func branchScopeKey(key string) bool {
	switch key {
	case "ref", "ref_name", "head_ref", "head_ref_name", "source_branch", "source_branch_name", "branch", "branch_name", "old_ref", "new_ref", "from_ref", "to_ref", "previous_ref", "current_ref", "old_branch", "new_branch", "previous_branch", "current_branch":
		return true
	default:
		return false
	}
}

func normalizeRecoveryBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	return strings.TrimPrefix(branch, "refs/remotes/origin/")
}

func recoveryJSONContainsSHA(raw json.RawMessage, preserved string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return recoveryValueContainsSHA(value, preserved)
}

func recoveryRefEventContainsSHA(raw json.RawMessage, sha string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return recoveryRefValueContainsSHA(value, sha)
}

func recoveryRefEventChanged(raw json.RawMessage, submitted string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	return recoveryRefValueChanged(value, submitted)
}

func recoveryRefValueContainsSHA(value any, sha string) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if recoveryRefValueContainsSHA(item, sha) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if recoveryRefSHAKey(key) {
				if candidate, ok := item.(string); ok && candidate == sha {
					return true
				}
			}
			if recoveryRefValueContainsSHA(item, sha) {
				return true
			}
		}
	}
	return false
}

func recoveryRefValueChanged(value any, submitted string) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if recoveryRefValueChanged(item, submitted) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if recoveryRefSHAKey(key) {
				if candidate, ok := item.(string); ok && recoverySHA(candidate) && candidate != submitted {
					return true
				}
			}
			if recoveryRefValueChanged(item, submitted) {
				return true
			}
		}
	}
	return false
}

func recoveryRefSHAKey(key string) bool {
	switch key {
	case "before", "after", "head", "head_sha", "commit_from", "commit_to", "oldrev", "newrev":
		return true
	default:
		return false
	}
}

func recoverySHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
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

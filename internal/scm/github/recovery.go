package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type recoveryPull struct {
	Number    int    `json:"number"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (h *Host) VerifyUnpublishedHistory(ctx context.Context, branch, submitted, preserved string, since, until int64, targetIdentity string) error {
	if err := h.Available(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(h.repo) == "" {
		return errors.New("GitHub repository identity is unavailable")
	}
	targetNumber := ""
	if strings.TrimSpace(targetIdentity) != "" {
		var err error
		targetNumber, err = scm.ExtractPRNumber(targetIdentity)
		if err != nil || (h.host != "" && scm.ExtractHost(targetIdentity) != "" && !strings.EqualFold(scm.ExtractHost(targetIdentity), h.host)) || RepoSlug(targetIdentity) != strings.TrimSpace(strings.TrimPrefix(h.repo, h.host+"/")) {
			return errors.New("GitHub submission-time pull-request identity is unavailable or mismatched")
		}
	}
	var pulls []recoveryPull
	if err := h.apiPages(ctx, "repos/"+h.apiRepoPath()+"/pulls?state=all&per_page=100", &pulls); err != nil {
		return fmt.Errorf("inspect GitHub pull-request history: %w", err)
	}
	matched := false
	for _, pull := range pulls {
		inWindow, err := recoveryRecordInWindow(pull.CreatedAt, pull.UpdatedAt, since, until)
		if err != nil {
			return fmt.Errorf("GitHub pull request %d has incomplete historical timestamps: %w", pull.Number, err)
		}
		if !inWindow {
			continue
		}
		if targetNumber != "" && fmt.Sprint(pull.Number) != targetNumber {
			continue
		}
		var events []json.RawMessage
		if err := h.apiPages(ctx, fmt.Sprintf("repos/%s/issues/%d/timeline?per_page=100", h.apiRepoPath(), pull.Number), &events); err != nil {
			return fmt.Errorf("inspect GitHub pull-request %d timeline: %w", pull.Number, err)
		}
		if targetNumber == "" && pull.Head.Ref != branch {
			related, explicit := recoveryBranchRelation(events, branch)
			if !related {
				if !explicit {
					return fmt.Errorf("GitHub pull request %d has incomplete submission-time target lineage", pull.Number)
				}
				continue
			}
		}
		matched = true
		if targetNumber != "" && (pull.Head.SHA == "" || pull.Head.SHA != submitted) {
			return fmt.Errorf("GitHub pull request %d has a changed head", pull.Number)
		}
		if pull.Head.SHA == preserved {
			return fmt.Errorf("GitHub pull request %d history contains the preserved unpublished head", pull.Number)
		}
		var commits []struct {
			SHA string `json:"sha"`
		}
		if err := h.apiPages(ctx, fmt.Sprintf("repos/%s/pulls/%d/commits?per_page=100", h.apiRepoPath(), pull.Number), &commits); err != nil {
			return fmt.Errorf("inspect GitHub pull-request %d commits: %w", pull.Number, err)
		}
		for _, commit := range commits {
			if commit.SHA == preserved {
				return fmt.Errorf("GitHub pull request %d history contains the preserved unpublished head", pull.Number)
			}
		}
		for _, event := range events {
			if recoveryJSONContainsSHA(event, preserved) {
				return fmt.Errorf("GitHub pull request %d history contains the preserved unpublished head", pull.Number)
			}
		}
	}
	if !matched && targetNumber != "" {
		return fmt.Errorf("GitHub pull request %s was not found in the submission interval", targetNumber)
	}
	return nil
}

func (h *Host) VerifyUnpublishedRefHistory(ctx context.Context, branch, submitted, preserved string, since, until int64) error {
	_, err := h.VerifyUnpublishedRefHistoryEvidence(ctx, branch, submitted, preserved, since, until)
	return err
}

func (h *Host) VerifyUnpublishedTargetHistory(ctx context.Context, branch, submitted, preserved string, since, until int64) (scm.HistoricalPublicationEvidence, error) {
	if err := h.VerifyUnpublishedHistory(ctx, branch, submitted, preserved, since, until, ""); err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	proof, err := h.VerifyUnpublishedRefHistoryEvidence(ctx, branch, submitted, preserved, since, until)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	refs, err := h.unpublishedTargetRequestRefs(ctx, branch, submitted, since, until)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	proof.RequestRefs = refs
	proof.Cursor += "|request-refs=" + strings.Join(refs, ",")
	return proof, nil
}

func (h *Host) unpublishedTargetRequestRefs(ctx context.Context, branch, submitted string, since, until int64) ([]string, error) {
	var pulls []recoveryPull
	if err := h.apiPages(ctx, "repos/"+h.apiRepoPath()+"/pulls?state=all&per_page=100", &pulls); err != nil {
		return nil, fmt.Errorf("inspect GitHub pull-request lineage: %w", err)
	}
	refs := make([]string, 0)
	for _, pull := range pulls {
		inWindow, err := recoveryRecordInWindow(pull.CreatedAt, pull.UpdatedAt, since, until)
		if err != nil {
			return nil, fmt.Errorf("GitHub pull request %d has incomplete historical timestamps: %w", pull.Number, err)
		}
		if !inWindow {
			continue
		}
		var events []json.RawMessage
		if err := h.apiPages(ctx, fmt.Sprintf("repos/%s/issues/%d/timeline?per_page=100", h.apiRepoPath(), pull.Number), &events); err != nil {
			return nil, fmt.Errorf("inspect GitHub pull-request %d lineage: %w", pull.Number, err)
		}
		related := pull.Head.Ref == branch
		if !related {
			var explicit bool
			related, explicit = recoveryBranchRelation(events, branch)
			if !related {
				if !explicit {
					return nil, fmt.Errorf("GitHub pull request %d has incomplete submission-time target lineage", pull.Number)
				}
				continue
			}
		}
		if pull.Head.SHA == "" || pull.Head.SHA != submitted {
			return nil, fmt.Errorf("GitHub pull request %d has a changed head", pull.Number)
		}
		refs = append(refs, fmt.Sprintf("refs/pull/%d/head", pull.Number))
	}
	sort.Strings(refs)
	return refs, nil
}

func (h *Host) VerifyUnpublishedRefHistoryEvidence(ctx context.Context, branch, submitted, preserved string, since, until int64) (scm.HistoricalPublicationEvidence, error) {
	if err := h.Available(ctx); err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	if strings.TrimSpace(h.repo) == "" {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitHub repository identity is unavailable")
	}
	var events []json.RawMessage
	endpoint := "repos/" + h.apiRepoPath() + "/events?per_page=100"
	pages, err := h.apiPagesWithCoverage(ctx, endpoint, &events)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, fmt.Errorf("inspect GitHub ref history: %w", err)
	}
	if pages == 0 || len(events) == 0 {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitHub ref history is incomplete")
	}
	for _, event := range events {
		inWindow, err := recoveryEventInWindow(event, since, until)
		if err != nil {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitHub ref history has incomplete timestamps: %w", err)
		}
		if !inWindow {
			continue
		}
		related, _ := recoveryBranchRelation([]json.RawMessage{event}, branch)
		if related {
			if recoveryRefEventContainsSHA(event, preserved) {
				return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitHub branch %s history contains the preserved unpublished head", branch)
			}
			if recoveryRefEventChanged(event, submitted) {
				return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitHub branch %s history contains a head other than the submitted head", branch)
			}
		}
	}
	cursor, err := recoveryEventCursor(events, since, until)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitHub ref history has incomplete coverage: %w", err)
	}
	return scm.HistoricalPublicationEvidence{
		Hash:     recoveryEvidenceHash(endpoint, branch, submitted, preserved, events),
		Cursor:   cursor,
		Coverage: fmt.Sprintf("github-events-pages=%d;events=%d;pagination=exhausted", pages, len(events)),
	}, nil
}

func recoveryEventCursor(events []json.RawMessage, since, until int64) (string, error) {
	min := int64(0)
	max := int64(0)
	for _, raw := range events {
		var event struct {
			CreatedAt string `json:"created_at"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return "", err
		}
		stamp := strings.TrimSpace(event.CreatedAt)
		if stamp == "" {
			stamp = strings.TrimSpace(event.Timestamp)
		}
		if stamp == "" {
			return "", errors.New("missing event timestamp")
		}
		when, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			return "", err
		}
		unix := when.Unix()
		if min == 0 || unix < min {
			min = unix
		}
		if unix > max {
			max = unix
		}
	}
	if since > 0 && min > since {
		return "", errors.New("event history does not cover the run interval")
	}
	return fmt.Sprintf("oldest=%d;newest=%d;since=%d;until=%d", min, max, since, until), nil
}

func recoveryEvidenceHash(endpoint, branch, submitted, preserved string, events []json.RawMessage) string {
	hash := sha256.New()
	for _, value := range []string{endpoint, branch, submitted, preserved} {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	for _, event := range events {
		hash.Write(event)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
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
	start := time.Unix(since, 0)
	end := time.Unix(until, 0)
	return !updatedAt.Before(start) && !createdAt.After(end), nil
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

func (h *Host) apiRepoPath() string {
	parts := strings.Split(strings.Trim(h.repo, "/"), "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		return strings.Join(parts[1:], "/")
	}
	return strings.Join(parts, "/")
}

func (h *Host) apiPages(ctx context.Context, endpoint string, dst interface{}) error {
	_, err := h.apiPagesWithCoverage(ctx, endpoint, dst)
	return err
}

func (h *Host) apiPagesWithCoverage(ctx context.Context, endpoint string, dst interface{}) (int, error) {
	args := []string{"api", "--paginate"}
	if h.host != "" && !strings.EqualFold(h.host, "github.com") {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	var merged []json.RawMessage
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return 0, err
		} else {
			merged = append(merged, raw)
		}
	}
	if len(merged) == 0 {
		return 0, errors.New("GitHub API returned no JSON")
	}
	var combined []byte
	if len(merged) == 1 {
		combined = merged[0]
	} else {
		var values []json.RawMessage
		for _, raw := range merged {
			var page []json.RawMessage
			if err := json.Unmarshal(raw, &page); err != nil {
				return 0, err
			}
			values = append(values, page...)
		}
		var err error
		combined, err = json.Marshal(values)
		if err != nil {
			return 0, err
		}
	}
	if err := json.Unmarshal(combined, dst); err != nil {
		return 0, err
	}
	return len(merged), nil
}

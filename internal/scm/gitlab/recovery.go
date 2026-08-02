package gitlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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
					continue
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
	_, err := h.VerifyUnpublishedRefHistoryEvidence(ctx, branch, submitted, preserved, since, until)
	return err
}

func (h *Host) VerifyUnpublishedTargetHistory(ctx context.Context, branch, submitted, preserved string, since, until int64) (scm.HistoricalPublicationEvidence, error) {
	return h.VerifyUnpublishedTargetHistoryAtCutoff(ctx, branch, submitted, preserved, since, until, 0)
}

func (h *Host) VerifyUnpublishedTargetHistoryAtCutoff(ctx context.Context, branch, submitted, preserved string, since, until, cutoff int64) (scm.HistoricalPublicationEvidence, error) {
	if err := h.VerifyUnpublishedHistory(ctx, branch, submitted, preserved, since, until, ""); err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	refs, err := h.unpublishedTargetRequestRefs(ctx, branch, submitted, since, until)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	proof, err := h.verifyUnpublishedAuditHistory(ctx, branch, submitted, preserved, refs, since, until, cutoff)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	proof.RequestRefs = refs
	proof.Cursor += "|request-refs=" + strings.Join(refs, ",")
	return proof, nil
}

func (h *Host) DiscoverSubmissionRequestRefs(ctx context.Context, branch, submitted string) ([]string, error) {
	if err := h.Available(ctx); err != nil {
		return nil, err
	}
	return h.unpublishedTargetRequestRefs(ctx, branch, submitted, 0, 0)
}

func (h *Host) unpublishedTargetRequestRefs(ctx context.Context, branch, submitted string, since, until int64) ([]string, error) {
	project := url.PathEscape(h.projectPath)
	var mergeRequests []recoveryMergeRequest
	if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests?state=all&per_page=100", project), &mergeRequests); err != nil {
		return nil, fmt.Errorf("inspect GitLab merge-request lineage: %w", err)
	}
	refs := make([]string, 0)
	for _, mergeRequest := range mergeRequests {
		var events []json.RawMessage
		if err := h.apiPages(ctx, fmt.Sprintf("projects/%s/merge_requests/%d/resource_state_events?per_page=100", project, mergeRequest.IID), &events); err != nil {
			return nil, fmt.Errorf("inspect GitLab merge-request %d lineage: %w", mergeRequest.IID, err)
		}
		related := mergeRequest.SourceBranch == branch
		if !related {
			var explicit bool
			related, explicit = recoveryBranchRelation(events, branch)
			if !related {
				if !explicit {
					continue
				}
				continue
			}
		}
		head := mergeRequest.SHA
		if head == "" {
			head = mergeRequest.DiffRefs.HeadSHA
		}
		if head == "" || head != submitted {
			return nil, fmt.Errorf("GitLab merge request %d has a changed head", mergeRequest.IID)
		}
		refs = append(refs, fmt.Sprintf("refs/merge-requests/%d/head", mergeRequest.IID))
	}
	sort.Strings(refs)
	return refs, nil
}

func (h *Host) verifyUnpublishedAuditHistory(ctx context.Context, branch, submitted, preserved string, requestRefs []string, since, until, cutoff int64) (scm.HistoricalPublicationEvidence, error) {
	if since <= 0 || until < since {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit history has no valid run interval")
	}
	if time.Now().Unix()-since > int64((180 * 24 * time.Hour).Seconds()) {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit history is outside the documented retention window")
	}
	if err := h.Available(ctx); err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	if strings.TrimSpace(h.projectPath) == "" {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit history requires a canonical project")
	}
	project := url.PathEscape(h.projectPath)
	query := url.Values{}
	query.Set("created_after", time.Unix(since, 0).UTC().Format(time.RFC3339))
	query.Set("per_page", "100")
	endpoint := "projects/" + project + "/audit_events?" + query.Encode()
	events, pages, pageCutoff, pageChain, err := h.gitlabAuditPages(ctx, endpoint, since, cutoff)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	if pages == 0 || pageCutoff <= 0 {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit event pagination is incomplete")
	}
	cutoff = pageCutoff
	if cutoff < since {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit event cutoff predates the run")
	}
	if !recoverySHA(submitted) || !recoverySHAForFormat(preserved, submitted) {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit history has noncanonical object IDs")
	}
	requestSet := make(map[string]struct{}, len(requestRefs))
	for _, ref := range requestRefs {
		requestSet[normalizeGitLabRef(ref)] = struct{}{}
	}
	relevantEvents := make([]json.RawMessage, 0, len(events))
	for _, event := range events {
		projectName := gitlabAuditField(event, "entity_path", "project_path", "project", "target_project")
		if projectName == "" {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit event has ambiguous project identity")
		}
		if !strings.EqualFold(strings.TrimSuffix(projectName, ".git"), h.projectPath) {
			continue
		}
		ref, targeted, ambiguous := gitlabAuditTargetRef(event, requestSet, branch)
		if ambiguous {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit event has ambiguous target identity")
		}
		if !targeted {
			continue
		}
		stamp, err := gitlabAuditEventTimestamp(event)
		if err != nil {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitLab audit event has incomplete timestamps: %w", err)
		}
		if stamp < since {
			continue
		}
		if stamp > cutoff {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit event history has a coverage gap")
		}
		if err := gitlabAuditValidateHeadValues(event, submitted); err != nil {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitLab audit event target %s has invalid head evidence: %w", ref, err)
		}
		if recoveryJSONContainsSHA(event, preserved) {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit event contains the preserved unpublished head")
		}
		if after := gitlabAuditField(event, "after", "commit_to", "newrev"); after != "" && recoverySHA(after) && !isZeroRecoverySHA(after) && after != submitted {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitLab audit event contains a changed target head")
		}
		relevantEvents = append(relevantEvents, event)
	}
	highWater := fmt.Sprintf("provider-date:%d", cutoff)
	return scm.HistoricalPublicationEvidence{
		Hash:      recoveryEvidenceHash(fmt.Sprintf("%s|cutoff=%d|pages=%s", endpoint, cutoff, pageChain), branch, submitted, preserved, relevantEvents),
		Cursor:    fmt.Sprintf("audit-cutoff=%d;since=%d;until=%d;pages=%s", cutoff, since, until, pageChain),
		Coverage:  fmt.Sprintf("gitlab-audit-pages=%d;events=%d;retention=180d;pagination=hasNextPage=false;empty-page-terminator;provider-date;audit", pages, len(relevantEvents)),
		HighWater: highWater,
		Complete:  true,
	}, nil
}

func gitlabAuditTargetRef(raw json.RawMessage, requestSet map[string]struct{}, branch string) (string, bool, bool) {
	refValue, _ := gitlabAuditRefField(raw, "ref", "branch", "head_ref", "source_ref", "source_branch", "target_ref")
	ref := normalizeGitLabRef(refValue)
	if number := gitlabAuditField(raw, "merge_request_iid", "merge_request_number", "merge_request_id", "request_iid"); number != "" {
		candidate := "refs/merge-requests/" + number + "/head"
		if _, ok := requestSet[candidate]; ok {
			return candidate, true, false
		}
		return candidate, false, true
	}
	if ref == "" {
		if gitlabAuditHasAnyField(raw, "ref", "branch", "head_ref", "source_ref", "source_branch", "target_ref", "old_ref", "new_ref", "from_ref", "to_ref", "previous_ref", "current_ref", "old_branch", "new_branch", "merge_request_iid", "merge_request_number", "merge_request_id", "request_iid") || (gitlabAuditHasAnyField(raw, "before", "after", "head", "head_sha", "head_commit_sha", "commit_sha", "commit_to", "oldrev", "newrev", "old_sha", "new_sha", "sha", "head_sha_after", "head_sha_before") && gitlabAuditLooksLikePublication(raw)) {
			return "", false, true
		}
		return "", false, false
	}
	if ref == normalizeGitLabRef(branch) {
		return ref, true, false
	}
	if _, ok := requestSet[ref]; ok {
		return ref, true, false
	}
	if strings.HasPrefix(ref, "refs/merge-requests/") || strings.HasPrefix(ref, "refs/heads/") {
		return ref, false, true
	}
	return ref, false, true
}

func gitlabAuditRefField(raw json.RawMessage, keys ...string) (string, string) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "", ""
	}
	var find func(any, string) (string, bool)
	find = func(item any, wanted string) (string, bool) {
		switch typed := item.(type) {
		case map[string]any:
			for key, child := range typed {
				if strings.ToLower(key) != wanted {
					continue
				}
				switch scalar := child.(type) {
				case string:
					if strings.TrimSpace(scalar) != "" {
						return scalar, true
					}
				case float64:
					return strconv.FormatInt(int64(scalar), 10), true
				}
			}
			for _, child := range typed {
				if found, ok := find(child, wanted); ok {
					return found, true
				}
			}
		case []any:
			for _, child := range typed {
				if found, ok := find(child, wanted); ok {
					return found, true
				}
			}
		}
		return "", false
	}
	for _, key := range keys {
		if found, ok := find(value, strings.ToLower(key)); ok {
			return found, strings.ToLower(key)
		}
	}
	return "", ""
}

func gitlabAuditValidateHeadValues(raw json.RawMessage, submitted string) error {
	fields, err := gitlabAuditHeadValues(raw)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return errors.New("no verifiable head")
	}
	for key, values := range fields {
		for _, value := range values {
			if !recoverySHAForFormat(value, submitted) {
				return fmt.Errorf("%s is not a canonical object ID", key)
			}
			if !isZeroRecoverySHA(value) && value != submitted {
				return fmt.Errorf("%s names a different head", key)
			}
		}
	}
	oldHead, newHead, hasPair, err := gitlabAuditHeadPair(fields)
	if err != nil {
		return err
	}
	action := strings.ToLower(gitlabAuditField(raw, "action", "action_name", "event", "event_type", "type", "operation"))
	switch {
	case strings.Contains(action, "create"):
		if !hasPair || !isZeroRecoverySHA(oldHead) || isZeroRecoverySHA(newHead) {
			return errors.New("create action lacks canonical zero-old/full-new head evidence")
		}
	case strings.Contains(action, "delete"):
		if !hasPair || isZeroRecoverySHA(oldHead) || !isZeroRecoverySHA(newHead) {
			return errors.New("delete action lacks canonical full-old/zero-new head evidence")
		}
	case strings.Contains(action, "rename"):
		if !hasPair || isZeroRecoverySHA(oldHead) || isZeroRecoverySHA(newHead) {
			return errors.New("rename action lacks canonical old/new head evidence")
		}
		if err := gitlabAuditValidateRefRename(raw); err != nil {
			return err
		}
	case strings.Contains(action, "push") || strings.Contains(action, "force") || strings.Contains(action, "update"):
		if !hasPair || isZeroRecoverySHA(oldHead) || isZeroRecoverySHA(newHead) {
			return errors.New("update action lacks canonical full old/new head evidence")
		}
	}
	return nil
}

func gitlabAuditHeadValues(raw json.RawMessage) (map[string][]string, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	wanted := map[string]struct{}{
		"before": {}, "after": {}, "head": {}, "head_sha": {}, "head_sha_before": {}, "head_sha_after": {},
		"commit_sha": {}, "commit_from": {}, "commit_to": {}, "head_commit_sha": {}, "oldrev": {}, "newrev": {}, "old_sha": {}, "new_sha": {},
		"sha": {},
	}
	fields := make(map[string][]string)
	var visit func(any) error
	visit = func(item any) error {
		switch typed := item.(type) {
		case map[string]any:
			for key, child := range typed {
				lower := strings.ToLower(key)
				if _, ok := wanted[lower]; ok {
					value, ok := child.(string)
					if !ok {
						return fmt.Errorf("%s is not a string", key)
					}
					fields[lower] = append(fields[lower], value)
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(value); err != nil {
		return nil, err
	}
	return fields, nil
}

func gitlabAuditHeadPair(fields map[string][]string) (string, string, bool, error) {
	var oldHead, newHead string
	found := false
	for _, pair := range [][2]string{{"before", "after"}, {"oldrev", "newrev"}, {"commit_from", "commit_to"}, {"head_sha_before", "head_sha_after"}, {"old_sha", "new_sha"}} {
		hasOld := len(fields[pair[0]]) > 0
		hasNew := len(fields[pair[1]]) > 0
		if !hasOld && !hasNew {
			continue
		}
		if hasOld != hasNew {
			return "", "", false, fmt.Errorf("head pair %s/%s is incomplete", pair[0], pair[1])
		}
		pairOld, pairNew := fields[pair[0]][0], fields[pair[1]][0]
		for _, value := range fields[pair[0]] {
			if value != pairOld {
				return "", "", false, fmt.Errorf("head field %s has conflicting values", pair[0])
			}
		}
		for _, value := range fields[pair[1]] {
			if value != pairNew {
				return "", "", false, fmt.Errorf("head field %s has conflicting values", pair[1])
			}
		}
		if found && (oldHead != pairOld || newHead != pairNew) {
			return "", "", false, errors.New("head evidence contains conflicting old/new pairs")
		}
		oldHead, newHead, found = pairOld, pairNew, true
	}
	return oldHead, newHead, found, nil
}

func gitlabAuditValidateRefRename(raw json.RawMessage) error {
	refs, err := gitlabAuditRefValues(raw)
	if err != nil {
		return err
	}
	var oldRef, newRef string
	found := false
	for _, pair := range [][2]string{{"old_ref", "new_ref"}, {"from_ref", "to_ref"}, {"previous_ref", "current_ref"}, {"old_branch", "new_branch"}} {
		hasOld := len(refs[pair[0]]) > 0
		hasNew := len(refs[pair[1]]) > 0
		if !hasOld && !hasNew {
			continue
		}
		if hasOld != hasNew {
			return fmt.Errorf("rename ref pair %s/%s is incomplete", pair[0], pair[1])
		}
		pairOld, err := gitlabAuditRenameRefValue(refs[pair[0]])
		if err != nil {
			return fmt.Errorf("rename action has invalid old ref: %w", err)
		}
		pairNew, err := gitlabAuditRenameRefValue(refs[pair[1]])
		if err != nil {
			return fmt.Errorf("rename action has invalid new ref: %w", err)
		}
		if pairOld == pairNew {
			return errors.New("rename action lacks canonical distinct old/new refs")
		}
		if found && (oldRef != pairOld || newRef != pairNew) {
			return errors.New("rename action contains conflicting ref aliases")
		}
		oldRef, newRef, found = pairOld, pairNew, true
	}
	if !found {
		return errors.New("rename action lacks canonical old/new refs")
	}
	return nil
}

func gitlabAuditRenameRefValue(values []string) (string, error) {
	if len(values) == 0 {
		return "", errors.New("rename ref is missing")
	}
	normalized := ""
	for _, value := range values {
		if !gitlabAuditCanonicalRef(value) {
			return "", fmt.Errorf("%q is not a canonical Git ref", value)
		}
		candidate := normalizeGitLabRef(value)
		if normalized != "" && normalized != candidate {
			return "", errors.New("rename ref has conflicting values")
		}
		normalized = candidate
	}
	return normalized, nil
}

func gitlabAuditRefValues(raw json.RawMessage) (map[string][]string, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	wanted := map[string]struct{}{
		"old_ref": {}, "new_ref": {}, "from_ref": {}, "to_ref": {}, "previous_ref": {}, "current_ref": {},
		"old_branch": {}, "new_branch": {},
	}
	refs := make(map[string][]string)
	var visit func(any) error
	visit = func(item any) error {
		switch typed := item.(type) {
		case map[string]any:
			for key, child := range typed {
				lower := strings.ToLower(key)
				if _, ok := wanted[lower]; ok {
					value, ok := child.(string)
					if !ok {
						return fmt.Errorf("%s is not a string", key)
					}
					refs[lower] = append(refs[lower], value)
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(value); err != nil {
		return nil, err
	}
	return refs, nil
}

func gitlabAuditCanonicalRef(ref string) bool {
	original := ref
	ref = strings.TrimSpace(ref)
	if ref == "" || original != ref || ref == "@" || (!strings.HasPrefix(ref, "refs/") && strings.HasPrefix(ref, "-")) || strings.ContainsAny(ref, "~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return false
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || component == "." || component == ".." || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
		for _, char := range component {
			if char <= 0x20 || char == 0x7f {
				return false
			}
		}
	}
	normalized := normalizeGitLabRef(ref)
	return normalized != "" && !strings.HasPrefix(normalized, "-")
}

func gitlabAuditLooksLikePublication(raw json.RawMessage) bool {
	action := strings.ToLower(gitlabAuditField(raw, "action", "action_name", "event", "event_type", "type", "operation"))
	return strings.Contains(action, "push") || strings.Contains(action, "force") || strings.Contains(action, "ref") || strings.Contains(action, "branch") || strings.Contains(action, "merge") || strings.Contains(action, "request") || strings.Contains(action, "create") || strings.Contains(action, "update") || strings.Contains(action, "delete") || strings.Contains(action, "rename")
}

func gitlabAuditHasAnyField(raw json.RawMessage, keys ...string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[strings.ToLower(key)] = struct{}{}
	}
	var visit func(any) bool
	visit = func(item any) bool {
		switch typed := item.(type) {
		case map[string]any:
			for key, child := range typed {
				if _, ok := wanted[strings.ToLower(key)]; ok {
					return true
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

type gitlabAuditPage struct {
	events     []json.RawMessage
	serverDate int64
	nextPage   int
}

func (h *Host) gitlabAuditPages(ctx context.Context, endpoint string, since, cutoff int64) ([]json.RawMessage, int, int64, string, error) {
	var events []json.RawMessage
	pageChain := make([]string, 0)
	pageCutoff := cutoff
	nextPage := 1
	seenPages := make(map[int]struct{})
	for pages := 0; pages < 10000; pages++ {
		pageNumber := nextPage
		if pageNumber <= 0 {
			return nil, 0, 0, "", errors.New("GitLab audit event history returned an invalid pagination cursor")
		}
		if _, seen := seenPages[pageNumber]; seen {
			return nil, 0, 0, "", errors.New("GitLab audit event history pagination repeated a cursor")
		}
		seenPages[pageNumber] = struct{}{}
		pageURL, err := url.Parse(endpoint)
		if err != nil {
			return nil, 0, 0, "", err
		}
		query := pageURL.Query()
		query.Set("page", strconv.Itoa(pageNumber))
		if pageCutoff > 0 {
			query.Set("created_before", time.Unix(pageCutoff, 0).UTC().Format(time.RFC3339))
		}
		pageURL.RawQuery = query.Encode()
		args := []string{"api", "--include"}
		if h.host != "" {
			args = append(args, "--hostname", h.host)
		}
		args = append(args, pageURL.String())
		out, err := h.cmd(ctx, "glab", args...).Output()
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("GitLab audit event history unavailable: %w", err)
		}
		if strings.Contains(strings.ToLower(string(out)), "rate limit") || strings.Contains(string(out), " 403 ") || strings.Contains(string(out), " 429 ") {
			return nil, 0, 0, "", errors.New("GitLab audit event history is unavailable or rate limited")
		}
		page, err := parseGitLabAuditPage(out)
		if err != nil {
			return nil, 0, 0, "", err
		}
		if pageCutoff == 0 {
			pageCutoff = page.serverDate
		}
		if page.serverDate < pageCutoff {
			return nil, 0, 0, "", errors.New("GitLab audit event history server time moved backwards")
		}
		pageChain = append(pageChain, strconv.Itoa(pageNumber))
		events = append(events, page.events...)
		if len(page.events) == 0 {
			if page.nextPage != 0 {
				return nil, 0, 0, "", errors.New("GitLab audit event history returned an empty page with a next cursor")
			}
			return events, pages + 1, pageCutoff, strings.Join(pageChain, ","), nil
		}
		if page.nextPage == 0 || page.nextPage <= pageNumber {
			return nil, 0, 0, "", errors.New("GitLab audit event history returned a nonempty page without an authoritative continuation cursor")
		}
		if page.nextPage != pageNumber+1 {
			return nil, 0, 0, "", errors.New("GitLab audit event history pagination has a cursor gap")
		}
		nextPage = page.nextPage
	}
	return nil, 0, 0, "", errors.New("GitLab audit event history pagination exceeded the safety bound")
}

func parseGitLabAuditPage(out []byte) (gitlabAuditPage, error) {
	start := bytes.IndexByte(out, '[')
	if start < 0 {
		return gitlabAuditPage{}, errors.New("GitLab audit event history returned no JSON page")
	}
	header := string(out[:start])
	statusOK := false
	serverDate := int64(0)
	nextPage := 0
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HTTP/") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return gitlabAuditPage{}, errors.New("GitLab audit event history returned an invalid HTTP status")
			}
			status, err := strconv.Atoi(fields[1])
			if err != nil || status < 200 || status >= 300 {
				return gitlabAuditPage{}, errors.New("GitLab audit event history returned a non-success status")
			}
			statusOK = true
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "date:") {
			when, err := http.ParseTime(strings.TrimSpace(line[len("date:"):]))
			if err != nil {
				return gitlabAuditPage{}, fmt.Errorf("GitLab audit event history returned an invalid server date: %w", err)
			}
			serverDate = when.Unix()
		}
		if strings.HasPrefix(lower, "x-next-page:") {
			value := strings.TrimSpace(line[len("x-next-page:"):])
			if value != "" {
				var err error
				nextPage, err = strconv.Atoi(value)
				if err != nil || nextPage <= 0 {
					return gitlabAuditPage{}, errors.New("GitLab audit event history returned an invalid next cursor")
				}
			}
		}
	}
	if !statusOK || serverDate <= 0 {
		return gitlabAuditPage{}, errors.New("GitLab audit event history did not expose an authoritative server date")
	}
	var events []json.RawMessage
	if err := json.Unmarshal(out[start:], &events); err != nil {
		return gitlabAuditPage{}, fmt.Errorf("decode GitLab audit event history page: %w", err)
	}
	return gitlabAuditPage{events: events, serverDate: serverDate, nextPage: nextPage}, nil
}

func gitlabIncludedAuditEvents(out []byte) ([]json.RawMessage, int, bool, error) {
	var events []json.RawMessage
	position := 0
	pages := 0
	complete := false
	headers := false
	for position < len(out) {
		relative := bytes.IndexAny(out[position:], "[{")
		if relative < 0 {
			break
		}
		start := position + relative
		header := string(out[position:start])
		if strings.Contains(header, "HTTP/") {
			headers = true
		}
		next := strings.Contains(strings.ToLower(header), `rel="next"`)
		for _, line := range strings.Split(header, "\n") {
			line = strings.TrimSpace(line)
			lowerLine := strings.ToLower(line)
			if strings.HasPrefix(lowerLine, "x-next-page:") && strings.TrimSpace(line[len("x-next-page:"):]) != "" {
				next = true
			}
		}
		decoder := json.NewDecoder(bytes.NewReader(out[start:]))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, 0, false, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, 0, false, err
		}
		events = append(events, page...)
		pages++
		complete = len(page) == 0 && !next
		position = start + int(decoder.InputOffset())
	}
	if !headers {
		return nil, 0, false, errors.New("GitLab audit event history did not expose pagination headers")
	}
	return events, pages, complete, nil
}

func gitlabAuditEventTimestamp(raw json.RawMessage) (int64, error) {
	value := gitlabAuditField(raw, "created_at", "timestamp", "createdAt")
	if value == "" {
		return 0, errors.New("missing event timestamp")
	}
	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		if number > 1_000_000_000_000 {
			return number / 1000, nil
		}
		return number, nil
	}
	when, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, err
	}
	return when.Unix(), nil
}

func gitlabAuditField(raw json.RawMessage, keys ...string) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[strings.ToLower(key)] = struct{}{}
	}
	var visit func(any) string
	visit = func(item any) string {
		switch typed := item.(type) {
		case map[string]any:
			for key, value := range typed {
				if _, ok := wanted[strings.ToLower(key)]; ok {
					switch scalar := value.(type) {
					case string:
						return scalar
					case float64:
						return strconv.FormatInt(int64(scalar), 10)
					}
				}
			}
			for _, value := range typed {
				if found := visit(value); found != "" {
					return found
				}
			}
		case []any:
			for _, value := range typed {
				if found := visit(value); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return visit(value)
}

func normalizeGitLabRef(ref string) string {
	ref = strings.TrimSpace(ref)
	return strings.TrimPrefix(ref, "refs/heads/")
}

func (h *Host) VerifyUnpublishedRefHistoryEvidence(ctx context.Context, branch, submitted, preserved string, since, until int64) (scm.HistoricalPublicationEvidence, error) {
	if err := h.Available(ctx); err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	if strings.TrimSpace(h.projectPath) == "" {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab project identity is unavailable")
	}
	project := url.PathEscape(h.projectPath)
	var events []json.RawMessage
	endpoint := fmt.Sprintf("projects/%s/events?per_page=100", project)
	pages, err := h.apiPagesWithCoverage(ctx, endpoint, &events)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, fmt.Errorf("inspect GitLab ref history: %w", err)
	}
	if pages == 0 || len(events) == 0 {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitLab ref history is incomplete")
	}
	for _, event := range events {
		inWindow, err := recoveryEventInWindow(event, since, until)
		if err != nil {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitLab ref history has incomplete timestamps: %w", err)
		}
		if !inWindow {
			continue
		}
		related, _ := recoveryBranchRelation([]json.RawMessage{event}, branch)
		if related {
			if recoveryRefEventContainsSHA(event, preserved) {
				return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitLab branch %s history contains the preserved unpublished head", branch)
			}
			if recoveryRefEventChanged(event, submitted) {
				return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitLab branch %s history contains a head other than the submitted head", branch)
			}
		}
	}
	cursor, err := recoveryEventCursor(events, since, until)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitLab ref history has incomplete coverage: %w", err)
	}
	return scm.HistoricalPublicationEvidence{
		Hash:     recoveryEvidenceHash(endpoint, branch, submitted, preserved, events),
		Cursor:   cursor,
		Coverage: fmt.Sprintf("gitlab-events-pages=%d;events=%d;pagination=exhausted", pages, len(events)),
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
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func recoverySHAForFormat(value, format string) bool {
	return recoverySHA(value) && (len(format) == 40 || len(format) == 64) && len(value) == len(format)
}

func isZeroRecoverySHA(value string) bool {
	for _, char := range value {
		if char != '0' {
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
	_, err := h.apiPagesWithCoverage(ctx, endpoint, dst)
	return err
}

func (h *Host) apiPagesWithCoverage(ctx context.Context, endpoint string, dst interface{}) (int, error) {
	args := []string{"api", "--paginate"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	cmd := h.cmd(ctx, "glab", args...)
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
		return 0, errors.New("GitLab API returned no JSON")
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

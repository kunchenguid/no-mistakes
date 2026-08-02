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
	"net/http"
	"net/url"
	"sort"
	"strconv"
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
					continue
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
	var pulls []recoveryPull
	if err := h.apiPages(ctx, "repos/"+h.apiRepoPath()+"/pulls?state=all&per_page=100", &pulls); err != nil {
		return nil, fmt.Errorf("inspect GitHub pull-request lineage: %w", err)
	}
	refs := make([]string, 0)
	for _, pull := range pulls {
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
					continue
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

func (h *Host) verifyUnpublishedAuditHistory(ctx context.Context, branch, submitted, preserved string, requestRefs []string, since, until, cutoff int64) (scm.HistoricalPublicationEvidence, error) {
	if since <= 0 || until < since {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitHub audit history has no valid run interval")
	}
	if time.Now().Unix()-since > int64((180 * 24 * time.Hour).Seconds()) {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitHub audit history is outside the documented retention window")
	}
	if err := h.Available(ctx); err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	owner, _, ok := strings.Cut(h.apiRepoPath(), "/")
	if !ok || owner == "" {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitHub audit history requires an organization repository")
	}
	query := url.Values{}
	query.Set("phrase", "repo:"+h.apiRepoPath())
	query.Set("include", "all")
	query.Set("after", strconv.FormatInt(since*1000, 10))
	query.Set("order", "asc")
	query.Set("per_page", "100")
	endpoint := "orgs/" + owner + "/audit-log?" + query.Encode()
	events, pages, pageCutoff, pageChain, err := h.githubAuditPages(ctx, endpoint, since, cutoff)
	if err != nil {
		return scm.HistoricalPublicationEvidence{}, err
	}
	if pages == 0 || pageCutoff <= 0 {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitHub organization audit log pagination is incomplete")
	}
	cutoff = pageCutoff
	if cutoff < since {
		return scm.HistoricalPublicationEvidence{}, errors.New("GitHub organization audit log cutoff predates the run")
	}
	requestSet := make(map[string]struct{}, len(requestRefs))
	for _, ref := range requestRefs {
		requestSet[normalizeGitHubRef(ref)] = struct{}{}
	}
	relevantEvents := make([]json.RawMessage, 0, len(events))
	for _, event := range events {
		repo := githubAuditField(event, "repo_name", "repository", "repo", "repository_name")
		if repo == "" {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitHub organization audit log record has ambiguous repository identity")
		}
		if !strings.EqualFold(strings.TrimSuffix(repo, ".git"), h.apiRepoPath()) {
			continue
		}
		ref, targeted, ambiguous := githubAuditTargetRef(event, requestSet, branch)
		if ambiguous {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitHub organization audit log record has ambiguous target identity")
		}
		if !targeted {
			continue
		}
		stamp, err := githubAuditEventTimestamp(event)
		if err != nil {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitHub organization audit log has incomplete timestamps: %w", err)
		}
		if stamp < since {
			continue
		}
		if stamp > cutoff {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitHub organization audit log has a coverage gap")
		}
		if githubAuditField(event, "before", "after", "head_sha", "commit_sha", "commit_to", "oldrev", "newrev", "sha", "head_sha_after", "head_sha_before") == "" {
			return scm.HistoricalPublicationEvidence{}, fmt.Errorf("GitHub organization audit log target %s has no verifiable head", ref)
		}
		if recoveryJSONContainsSHA(event, preserved) {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitHub organization audit log contains the preserved unpublished head")
		}
		if after := githubAuditField(event, "after", "commit_to", "newrev"); after != "" && recoverySHA(after) && after != submitted {
			return scm.HistoricalPublicationEvidence{}, errors.New("GitHub organization audit log contains a changed target head")
		}
		relevantEvents = append(relevantEvents, event)
	}
	highWater := fmt.Sprintf("provider-date:%d", cutoff)
	return scm.HistoricalPublicationEvidence{
		Hash:      recoveryEvidenceHash(fmt.Sprintf("%s|cutoff=%d|pages=%s", endpoint, cutoff, pageChain), branch, submitted, preserved, relevantEvents),
		Cursor:    fmt.Sprintf("audit-cutoff=%d;since=%d;until=%d;pages=%s", cutoff, since, until, pageChain),
		Coverage:  fmt.Sprintf("github-org-audit-pages=%d;events=%d;retention=180d;pagination=hasNextPage=false;empty-page-terminator;provider-date;audit", pages, len(relevantEvents)),
		HighWater: highWater,
		Complete:  true,
	}, nil
}

func githubAuditTargetRef(raw json.RawMessage, requestSet map[string]struct{}, branch string) (string, bool, bool) {
	ref := normalizeGitHubRef(githubAuditField(raw, "ref", "branch", "head_ref", "source_ref", "source_branch", "target_ref"))
	if number := githubAuditField(raw, "pull_request_number", "pull_number", "pull_request_id", "request_number"); number != "" {
		candidate := "refs/pull/" + number + "/head"
		if _, ok := requestSet[candidate]; ok {
			return candidate, true, false
		}
		if ref == "" {
			return candidate, false, false
		}
	}
	if ref == "" {
		if githubAuditHasAnyField(raw, "ref", "branch", "head_ref", "source_ref", "source_branch", "target_ref", "pull_request_number", "pull_number", "pull_request_id", "request_number") || (githubAuditHasAnyField(raw, "before", "after", "head_sha", "commit_sha", "commit_to", "oldrev", "newrev", "sha", "head_sha_after", "head_sha_before") && githubAuditLooksLikePublication(raw)) {
			return "", false, true
		}
		return "", false, false
	}
	if ref == normalizeGitHubRef(branch) {
		return ref, true, false
	}
	if _, ok := requestSet[ref]; ok {
		return ref, true, false
	}
	return ref, false, false
}

func githubAuditLooksLikePublication(raw json.RawMessage) bool {
	action := strings.ToLower(githubAuditField(raw, "action", "action_name", "event", "event_type", "type", "operation"))
	return strings.Contains(action, "push") || strings.Contains(action, "force") || strings.Contains(action, "ref") || strings.Contains(action, "branch") || strings.Contains(action, "pull") || strings.Contains(action, "request") || strings.Contains(action, "create") || strings.Contains(action, "update") || strings.Contains(action, "delete") || strings.Contains(action, "rename")
}

func githubAuditHasAnyField(raw json.RawMessage, keys ...string) bool {
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

type githubAuditPage struct {
	events     []json.RawMessage
	serverDate int64
	nextPage   int
}

func (h *Host) githubAuditPages(ctx context.Context, endpoint string, since, cutoff int64) ([]json.RawMessage, int, int64, string, error) {
	var events []json.RawMessage
	pageChain := make([]string, 0)
	pageCutoff := cutoff
	for pageNumber := 1; pageNumber <= 10000; pageNumber++ {
		pageURL, err := url.Parse(endpoint)
		if err != nil {
			return nil, 0, 0, "", err
		}
		query := pageURL.Query()
		query.Set("page", strconv.Itoa(pageNumber))
		if pageCutoff > 0 {
			query.Set("before", strconv.FormatInt(pageCutoff*1000, 10))
		}
		pageURL.RawQuery = query.Encode()
		args := []string{"api", "--include"}
		if h.host != "" && !strings.EqualFold(h.host, "github.com") {
			args = append(args, "--hostname", h.host)
		}
		args = append(args, pageURL.String())
		out, err := h.cmd(ctx, "gh", args...).Output()
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("GitHub organization audit log unavailable: %w", err)
		}
		if strings.Contains(strings.ToLower(string(out)), "rate limit") || strings.Contains(string(out), " 403 ") || strings.Contains(string(out), " 429 ") {
			return nil, 0, 0, "", errors.New("GitHub organization audit log is unavailable or rate limited")
		}
		page, err := parseGitHubAuditPage(out)
		if err != nil {
			return nil, 0, 0, "", err
		}
		if pageCutoff == 0 {
			pageCutoff = page.serverDate
		}
		if page.serverDate < pageCutoff {
			return nil, 0, 0, "", errors.New("GitHub organization audit log server time moved backwards")
		}
		pageChain = append(pageChain, strconv.Itoa(pageNumber))
		if page.nextPage != 0 && page.nextPage != pageNumber+1 {
			return nil, 0, 0, "", errors.New("GitHub organization audit log pagination has a cursor gap")
		}
		events = append(events, page.events...)
		if len(page.events) == 0 {
			if page.nextPage != 0 {
				return nil, 0, 0, "", errors.New("GitHub organization audit log returned an empty page with a next cursor")
			}
			return events, pageNumber, pageCutoff, strings.Join(pageChain, ","), nil
		}
	}
	return nil, 0, 0, "", errors.New("GitHub organization audit log pagination exceeded the safety bound")
}

func parseGitHubAuditPage(out []byte) (githubAuditPage, error) {
	start := bytes.IndexByte(out, '[')
	if start < 0 {
		return githubAuditPage{}, errors.New("GitHub organization audit log returned no JSON page")
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
				return githubAuditPage{}, errors.New("GitHub organization audit log returned an invalid HTTP status")
			}
			status, err := strconv.Atoi(fields[1])
			if err != nil || status < 200 || status >= 300 {
				return githubAuditPage{}, errors.New("GitHub organization audit log returned a non-success status")
			}
			statusOK = true
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "date:") {
			when, err := http.ParseTime(strings.TrimSpace(line[len("date:"):]))
			if err != nil {
				return githubAuditPage{}, fmt.Errorf("GitHub organization audit log returned an invalid server date: %w", err)
			}
			serverDate = when.Unix()
		}
		if strings.HasPrefix(lower, "link:") && strings.Contains(lower, `rel="next"`) {
			startURL := strings.Index(line, "<")
			endURL := strings.Index(line[startURL+1:], ">")
			if startURL < 0 || endURL < 0 {
				return githubAuditPage{}, errors.New("GitHub organization audit log returned an invalid next cursor")
			}
			nextURL, err := url.Parse(line[startURL+1 : startURL+1+endURL])
			if err != nil {
				return githubAuditPage{}, errors.New("GitHub organization audit log returned an invalid next cursor")
			}
			nextPage, err = strconv.Atoi(nextURL.Query().Get("page"))
			if err != nil || nextPage <= 0 {
				return githubAuditPage{}, errors.New("GitHub organization audit log returned an invalid next cursor")
			}
		}
	}
	if !statusOK || serverDate <= 0 {
		return githubAuditPage{}, errors.New("GitHub organization audit log did not expose an authoritative server date")
	}
	var events []json.RawMessage
	if err := json.Unmarshal(out[start:], &events); err != nil {
		return githubAuditPage{}, fmt.Errorf("decode GitHub organization audit log page: %w", err)
	}
	return githubAuditPage{events: events, serverDate: serverDate, nextPage: nextPage}, nil
}

func githubIncludedAuditEvents(out []byte) ([]json.RawMessage, int, bool, error) {
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
		return nil, 0, false, errors.New("GitHub organization audit log did not expose pagination headers")
	}
	return events, pages, complete, nil
}

func githubAuditEventTimestamp(raw json.RawMessage) (int64, error) {
	value := githubAuditField(raw, "@timestamp", "timestamp", "created_at", "createdAt")
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

func githubAuditField(raw json.RawMessage, keys ...string) string {
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

func normalizeGitHubRef(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "refs/heads/")
	return ref
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

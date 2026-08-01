package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		if targetNumber == "" && pull.Head.Ref != branch {
			continue
		}
		matched = true
		if pull.Head.SHA == "" || pull.Head.SHA != submitted {
			return fmt.Errorf("GitHub pull request %d has a changed head", pull.Number)
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
		var events []json.RawMessage
		if err := h.apiPages(ctx, fmt.Sprintf("repos/%s/issues/%d/timeline?per_page=100", h.apiRepoPath(), pull.Number), &events); err != nil {
			return fmt.Errorf("inspect GitHub pull-request %d timeline: %w", pull.Number, err)
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

func (h *Host) apiRepoPath() string {
	parts := strings.Split(strings.Trim(h.repo, "/"), "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		return strings.Join(parts[1:], "/")
	}
	return strings.Join(parts, "/")
}

func (h *Host) apiPages(ctx context.Context, endpoint string, dst interface{}) error {
	args := []string{"api", "--paginate"}
	if h.host != "" && !strings.EqualFold(h.host, "github.com") {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	cmd := h.cmd(ctx, "gh", args...)
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
		return errors.New("GitHub API returned no JSON")
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

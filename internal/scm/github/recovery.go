package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type recoveryPull struct {
	Number int `json:"number"`
	Head   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (h *Host) VerifyUnpublishedHistory(ctx context.Context, branch, submitted, preserved string) error {
	if err := h.Available(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(h.repo) == "" {
		return errors.New("GitHub repository identity is unavailable")
	}
	var pulls []recoveryPull
	if err := h.apiPages(ctx, "repos/"+h.apiRepoPath()+"/pulls?state=all&per_page=100", &pulls); err != nil {
		return fmt.Errorf("inspect GitHub pull-request history: %w", err)
	}
	for _, pull := range pulls {
		if pull.Head.Ref != branch {
			continue
		}
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
		var events []struct {
			SHA string `json:"sha"`
		}
		if err := h.apiPages(ctx, fmt.Sprintf("repos/%s/issues/%d/timeline?per_page=100", h.apiRepoPath(), pull.Number), &events); err != nil {
			return fmt.Errorf("inspect GitHub pull-request %d timeline: %w", pull.Number, err)
		}
		for _, event := range events {
			if event.SHA == preserved {
				return fmt.Errorf("GitHub pull request %d history contains the preserved unpublished head", pull.Number)
			}
		}
	}
	return nil
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

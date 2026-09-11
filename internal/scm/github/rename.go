package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// parseDiscoveredPRURL permits a renamed repository only with authenticated
// stable identity proof. A redirect or the returned PR URL alone is not proof.
func (h *Host) parseDiscoveredPRURL(ctx context.Context, raw string) (int, error) {
	host := h.host
	number, err := parsePullRequestURL(raw, host, h.repoSlug())
	if err == nil {
		return number, nil
	}
	if host == "" {
		return 0, fmt.Errorf("cannot verify renamed GitHub repository without a known host: %w", err)
	}
	// Validate every other URL property before making any additional request.
	number, shapeErr := parsePullRequestURL(raw, host, "")
	if shapeErr != nil {
		return 0, shapeErr
	}
	canonical := RepoSlug(raw)
	expected, err := h.repositoryIdentity(ctx, host, h.repoSlug())
	if err != nil {
		return 0, err
	}
	actual, err := h.repositoryIdentity(ctx, host, canonical)
	if err != nil {
		return 0, err
	}
	if expected.ID != actual.ID || !strings.EqualFold(expected.FullName, canonical) || !strings.EqualFold(actual.FullName, canonical) {
		return 0, fmt.Errorf("URL repository %q does not match verified GitHub repository identity", canonical)
	}
	return number, nil
}

type repositoryIdentity struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

func (h *Host) repositoryIdentity(ctx context.Context, host, slug string) (repositoryIdentity, error) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return repositoryIdentity{}, fmt.Errorf("invalid GitHub repository identity selector")
	}
	endpoint := "repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	out, err := h.cmd(ctx, "gh", "api", "--hostname", host, endpoint).Output()
	if err != nil {
		// Do not echo provider output, which may contain credential material.
		return repositoryIdentity{}, fmt.Errorf("verify GitHub repository identity: %w", err)
	}
	var identity repositoryIdentity
	if err := json.Unmarshal(out, &identity); err != nil || identity.ID <= 0 || identity.FullName == "" {
		return repositoryIdentity{}, fmt.Errorf("invalid GitHub repository identity response")
	}
	return identity, nil
}

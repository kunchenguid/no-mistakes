package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// RepoParent reports whether remoteURL's GitHub repository is a fork and, if
// so, its parent's "owner/name" slug, read from `gh api repos/<owner>/<name>`.
// cmd is caller-supplied so this can run before a repository is registered and
// a full Host constructed (e.g. from `init`), and so tests can substitute a
// fake CLI instead of shelling out for real. host scopes the call to a GitHub
// Enterprise Server hostname the same way callers of the Host type do; pass ""
// for github.com.
//
// An empty parentSlug with isFork false means remoteURL's repository is not a
// fork. Every failure (gh unavailable, unauthenticated, offline, or an
// unparseable response) is returned as an error rather than folded into a
// false isFork, so a caller doing security-relevant fork-layout detection can
// tell "confirmed not a fork" from "could not check" and fail open on the
// latter.
func RepoParent(ctx context.Context, cmd CmdFactory, host, remoteURL string) (parentSlug string, isFork bool, err error) {
	slug := RepoSlug(remoteURL)
	if slug == "" {
		return "", false, fmt.Errorf("resolve GitHub repository for %q", remoteURL)
	}
	args := []string{"api"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	args = append(args, "repos/"+slug)
	out, err := cmd(ctx, "gh", args...).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", false, fmt.Errorf("gh api repos/%s: %s: %w", slug, strings.TrimSpace(string(exitErr.Stderr)), err)
		}
		return "", false, fmt.Errorf("gh api repos/%s: %w", slug, err)
	}
	var payload struct {
		Fork   bool `json:"fork"`
		Parent struct {
			FullName string `json:"full_name"`
		} `json:"parent"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", false, fmt.Errorf("parse gh api repos/%s response: %w", slug, err)
	}
	return strings.TrimSpace(payload.Parent.FullName), payload.Fork, nil
}

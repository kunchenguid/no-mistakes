package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// ProbeCIConfiguration reports whether this repository has any GitHub Actions
// workflow that could still report a check for headSHA.
//
// It exists because an empty check rollup is ambiguous. A repository whose
// workflows have not registered a run yet and a repository that has no CI at
// all both report zero checks, and only the second one will never report any.
// GetChecks cannot tell them apart, so a caller that waits for checks to appear
// on a repository with no workflows waits forever.
//
// Two independent sources answer the question, and either one alone is enough
// to call CI present:
//
//   - the workflows GitHub has registered for the repository, which covers
//     every repository whose CI is already established; and
//   - the workflow files in the commit's own tree, which covers the pull
//     request that ADDS the first workflow. GitHub does not list a workflow
//     until it creates the first run for it, so the registered list is empty in
//     exactly the window this probe runs in.
//
// Absent is therefore reported only when GitHub reports no registered workflow
// AND the commit's tree defines none either. Every read that cannot be
// completed returns CIConfigurationUnknown with an error, so the caller keeps
// waiting rather than acting on a guess.
//
// Bound worth knowing: this probe sees GitHub Actions only. A repository whose
// checks are posted by an external service through the Statuses API is
// indistinguishable from one with no CI until that service posts its first
// status for the commit. Absent is therefore evidence that waiting cannot end,
// never evidence that the commit passed, and callers must report it as an
// unresolved outcome rather than a green one.
func (h *Host) ProbeCIConfiguration(ctx context.Context, _ *scm.PR, headSHA string) (scm.CIConfiguration, error) {
	slug := h.apiRepoSlug()
	if slug == "" {
		return scm.CIConfigurationUnknown, errors.New("no GitHub repository is known; cannot determine whether CI is configured")
	}

	registered, err := h.registeredWorkflowCount(ctx, slug)
	if err != nil {
		return scm.CIConfigurationUnknown, err
	}
	if registered > 0 {
		return scm.CIConfigurationPresent, nil
	}

	defined, err := h.commitDefinesWorkflows(ctx, slug, headSHA)
	if err != nil {
		return scm.CIConfigurationUnknown, err
	}
	if defined {
		return scm.CIConfigurationPresent, nil
	}
	return scm.CIConfigurationAbsent, nil
}

// apiRepoSlug returns the bare "owner/name" a REST path needs. h.repo carries
// the host prefix that gh's --repo flag requires on GitHub Enterprise Server,
// which a REST path must not contain; the instance is named by --hostname
// instead. It returns "" when no repository is known, so the caller can fail
// closed rather than send gh a path it would resolve against the wrong repo.
func (h *Host) apiRepoSlug() string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(h.repo), "/"), "/")
	switch len(parts) {
	case 2:
		return parts[0] + "/" + parts[1]
	case 3:
		return parts[1] + "/" + parts[2]
	default:
		return ""
	}
}

// apiArgs builds a `gh api` invocation for path, scoped to this repository's
// host for the same reason Available scopes its auth check: a gh configuration
// with several GitHub instances must not answer for the wrong one.
func (h *Host) apiArgs(path string) []string {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	return append(args, path)
}

// registeredWorkflowCount returns how many workflows GitHub has registered for
// the repository. Workflows in every state count: a workflow disabled for
// inactivity can be re-enabled and its definition still exists, so it is not
// evidence that the repository has no CI.
func (h *Host) registeredWorkflowCount(ctx context.Context, slug string) (int, error) {
	out, err := h.cmd(ctx, "gh", h.apiArgs("repos/"+slug+"/actions/workflows")...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("gh api actions/workflows: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload struct {
		TotalCount int               `json:"total_count"`
		Workflows  []json.RawMessage `json:"workflows"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return 0, fmt.Errorf("parse registered workflows: %w", err)
	}
	// total_count is the whole-repository count and so survives pagination, but
	// a response that omitted it must not read as zero while workflows are
	// listed: the larger of the two is the safe reading in both directions.
	if payload.TotalCount > len(payload.Workflows) {
		return payload.TotalCount, nil
	}
	return len(payload.Workflows), nil
}

// commitDefinesWorkflows reports whether the commit's own tree carries a
// workflow definition. headSHA pins the answer to the commit under test so a
// pull request that adds the first workflow is recognized; when it is unknown
// the repository's default branch answers instead.
func (h *Host) commitDefinesWorkflows(ctx context.Context, slug, headSHA string) (bool, error) {
	path := "repos/" + slug + "/contents/.github/workflows"
	if ref := strings.TrimSpace(headSHA); ref != "" {
		path += "?ref=" + url.QueryEscape(ref)
	}
	out, err := h.cmd(ctx, "gh", h.apiArgs(path)...).CombinedOutput()
	if err != nil {
		// A repository with no workflow directory answers 404. That is the
		// evidence this probe is looking for, not a failure to read.
		if isNotFound(out) {
			return false, nil
		}
		return false, fmt.Errorf("gh api contents/.github/workflows: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return false, fmt.Errorf("parse workflow directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Type != "" && entry.Type != "file" {
			continue
		}
		if isWorkflowDefinition(entry.Name) {
			return true, nil
		}
	}
	return false, nil
}

// isWorkflowDefinition reports whether a file in .github/workflows is one
// GitHub would read as a workflow. Anything else in that directory (a README,
// a shared script) defines no CI.
func isWorkflowDefinition(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml")
}

// isNotFound recognizes gh's own report of a 404, which is how the CLI conveys
// "this path does not exist" for an otherwise successful, authenticated call.
func isNotFound(out []byte) bool {
	return strings.Contains(string(out), "HTTP 404")
}

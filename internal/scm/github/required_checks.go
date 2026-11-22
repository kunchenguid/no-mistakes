package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// rulesetsUnavailableMessage is the 403 body GitHub returns from the branch
// rules endpoint when the repository's plan has no rulesets at all (a private
// repository on a free plan). That refusal is itself the answer - no ruleset
// can require a check there - so it is the one rules-read error read as "none".
// Any other refusal, including a reworded one, stays an error and keeps the
// caller waiting.
const rulesetsUnavailableMessage = "Upgrade to GitHub Pro or make this repository public to enable this feature"

// RequiredChecks returns what the forge requires before a pull request into
// baseBranch can merge: the status checks named by classic branch protection
// and by every active ruleset for that branch (repository or organization),
// plus the ruleset rules that are satisfied by checks GitHub runs itself
// (required workflows and code scanning). An empty result is positive
// evidence that GitHub requires no check; any read or decode failure is an
// error, never an empty result.
func (h *Host) RequiredChecks(ctx context.Context, baseBranch string) ([]string, error) {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return nil, errors.New("required checks: no base branch")
	}
	repo := h.repoSlug()
	if repo == "" {
		return nil, errors.New("required checks: repository is unknown")
	}
	branchPath := escapeBranchPath(baseBranch)
	required := map[string]bool{}

	protected, err := h.ghAPI(ctx, "repos/"+repo+"/branches/"+branchPath)
	if err != nil {
		return nil, fmt.Errorf("read branch protection for %s: %w", baseBranch, err)
	}
	var branch struct {
		Protection *struct {
			RequiredStatusChecks *struct {
				EnforcementLevel string   `json:"enforcement_level"`
				Contexts         []string `json:"contexts"`
				Checks           []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(protected, &branch); err != nil {
		return nil, fmt.Errorf("parse branch protection for %s: %w", baseBranch, err)
	}
	if branch.Name == "" {
		return nil, fmt.Errorf("parse branch protection for %s: response names no branch", baseBranch)
	}
	if branch.Protection != nil && branch.Protection.RequiredStatusChecks != nil {
		checks := branch.Protection.RequiredStatusChecks
		if checks.EnforcementLevel != "off" {
			for _, name := range checks.Contexts {
				required[name] = true
			}
			for _, check := range checks.Checks {
				required[check.Context] = true
			}
		}
	}

	rules, err := h.ghAPI(ctx, "repos/"+repo+"/rules/branches/"+branchPath, "--paginate", "--slurp")
	if err != nil {
		if !strings.Contains(err.Error(), rulesetsUnavailableMessage) {
			return nil, fmt.Errorf("read rulesets for %s: %w", baseBranch, err)
		}
		rules = nil
	}
	if rules != nil {
		var pages [][]struct {
			Type       string `json:"type"`
			Parameters struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(rules, &pages); err != nil {
			return nil, fmt.Errorf("parse rulesets for %s: %w", baseBranch, err)
		}
		for _, page := range pages {
			for _, rule := range page {
				switch rule.Type {
				case "required_status_checks":
					for _, check := range rule.Parameters.RequiredStatusChecks {
						required[check.Context] = true
					}
				case "workflows", "code_scanning":
					required["ruleset rule "+rule.Type] = true
				}
			}
		}
	}

	delete(required, "")
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (h *Host) ghAPI(ctx context.Context, endpoint string, extra ...string) ([]byte, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint)
	args = append(args, extra...)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %s: %w", endpoint, strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

// escapeBranchPath escapes each segment of a branch name for a REST path
// while keeping the separators GitHub expects between them.
func escapeBranchPath(branch string) string {
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

package azuredevops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const prFactsPageSize = 100

// sourceRepositoryURL returns one stable identity for Azure HTTPS and SSH
// remotes. PRFacts uses the browsable repository URL; callers may pass their
// configured push remote to discovery.
func sourceRepositoryURL(raw string) (string, string, string, string, error) {
	org, project, repo, ok := ParseRemote(raw)
	if !ok {
		return "", "", "", "", fmt.Errorf("invalid Azure source repository %q", raw)
	}
	return webPRURL(org, project, repo, "", ""), org, project, repo, nil
}

// CanonicalSourceRepository removes transport syntax and credentials from an
// Azure remote before its identity is compared with or persisted beside PR
// facts. The API reports this same browsable repository URL.
func CanonicalSourceRepository(raw string) (string, error) {
	canonical, _, _, _, err := sourceRepositoryURL(raw)
	return canonical, err
}

func azBranch(raw string) (string, error) {
	if !strings.HasPrefix(raw, "refs/heads/") {
		return "", fmt.Errorf("invalid Azure branch ref %q", raw)
	}
	branch := strings.TrimPrefix(raw, "refs/heads/")
	if branch == "" || branch != strings.TrimSpace(branch) {
		return "", fmt.Errorf("invalid Azure branch ref %q", raw)
	}
	return branch, nil
}

func azFullSHA(raw string) bool {
	if len(raw) != 40 {
		return false
	}
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			if ch < 'a' || ch > 'f' {
				return false
			}
		}
	}
	return true
}

// sourceFromMetadata resolves a fork's own repository. A forkSource object
// without a usable repository cannot silently become the target repository.
func (h *Host) sourceFromMetadata(repo azRepository) (string, error) {
	if strings.TrimSpace(repo.WebURL) != "" {
		org, project, name, err := parseRepositoryWebURL(repo.WebURL)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(azureOrganizationName(org), azureOrganizationName(h.org)) {
			return "", fmt.Errorf("fork repository organization %q does not match configured organization", org)
		}
		if metadata := strings.TrimSpace(repo.Name); metadata != "" && !strings.EqualFold(metadata, name) {
			return "", fmt.Errorf("fork repository name disagrees with webUrl")
		}
		if metadata := strings.TrimSpace(repo.Project.Name); metadata != "" && !strings.EqualFold(metadata, project) {
			return "", fmt.Errorf("fork project name disagrees with webUrl")
		}
		return webPRURL(h.org, project, name, "", ""), nil
	}
	project, name := strings.TrimSpace(repo.Project.Name), strings.TrimSpace(repo.Name)
	if project == "" || name == "" {
		return "", fmt.Errorf("fork source lacks repository project or name")
	}
	return webPRURL(h.org, project, name, "", ""), nil
}

func (h *Host) factsFromPR(raw azPR) (scm.PRFacts, error) {
	if raw.PullRequestID <= 0 {
		return scm.PRFacts{}, fmt.Errorf("Azure PR has no positive pullRequestId")
	}
	if err := h.validateListedPR(raw); err != nil {
		return scm.PRFacts{}, err
	}
	var state scm.PRState
	switch strings.ToLower(strings.TrimSpace(raw.Status)) {
	case "active":
		state = scm.PRStateOpen
	case "completed":
		state = scm.PRStateMerged
	case "abandoned":
		state = scm.PRStateClosed
	default:
		return scm.PRFacts{}, fmt.Errorf("Azure PR %d has invalid status %q", raw.PullRequestID, raw.Status)
	}
	source, err := azBranch(raw.SourceRefName)
	if err != nil {
		return scm.PRFacts{}, err
	}
	base, err := azBranch(raw.TargetRefName)
	if err != nil {
		return scm.PRFacts{}, err
	}
	head := strings.TrimSpace(raw.LastMergeSourceCommit.CommitID)
	if !azFullSHA(head) {
		return scm.PRFacts{}, fmt.Errorf("Azure PR %d lacks a full source commit SHA", raw.PullRequestID)
	}
	sourceRepository := webPRURL(h.org, h.project, h.repo, "", "")
	if raw.ForkSource != nil {
		sourceRepository, err = h.sourceFromMetadata(raw.ForkSource.Repository)
		if err != nil {
			return scm.PRFacts{}, err
		}
	}
	id := strconv.Itoa(raw.PullRequestID)
	pr := scm.PR{Number: id, URL: webPRURL(h.org, h.project, h.repo, "", id), HeadSHA: head, BaseBranch: base}
	return scm.PRFacts{PR: pr, State: state, SourceRepository: sourceRepository, SourceBranch: source, HeadSHA: head, BaseBranch: base}, nil
}

// lastMergeSourceCommit is a PR merge-computation snapshot, which can lag a
// push. For an active PR, require it to agree with the live source ref before
// using it as a head binding.
func (h *Host) bindLiveHead(ctx context.Context, facts scm.PRFacts, cache map[string]string) (scm.PRFacts, error) {
	if facts.State != scm.PRStateOpen {
		return facts, nil // a completed PR's source branch may already be deleted
	}
	key := facts.SourceRepository + "\x00" + facts.SourceBranch
	live, cached := cache[key]
	if !cached {
		_, org, project, repo, err := sourceRepositoryURL(facts.SourceRepository)
		if err != nil || !strings.EqualFold(azureOrganizationName(org), azureOrganizationName(h.org)) {
			return scm.PRFacts{}, fmt.Errorf("Azure PR has an invalid source repository")
		}
		args := []string{"repos", "ref", "list", "--filter", "heads/" + facts.SourceBranch, "--organization", h.org, "--project", project, "--repository", repo, "--output", "json"}
		out, err := outputJSON(h.cmd(ctx, "az", args...))
		if err != nil {
			return scm.PRFacts{}, fmt.Errorf("az repos ref list: %w", err)
		}
		var refs []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &refs); err != nil || refs == nil {
			return scm.PRFacts{}, fmt.Errorf("az repos ref list: invalid array: %v", err)
		}
		for _, ref := range refs {
			if ref.Name != "refs/heads/"+facts.SourceBranch {
				continue // Azure's filter is a prefix match
			}
			if live != "" || !azFullSHA(ref.ObjectID) {
				return scm.PRFacts{}, fmt.Errorf("az repos ref list: duplicate or invalid source ref")
			}
			live = ref.ObjectID
		}
		if live == "" {
			return scm.PRFacts{}, fmt.Errorf("az repos ref list: source branch is absent")
		}
		cache[key] = live
	}
	if facts.HeadSHA != live {
		return scm.PRFacts{}, fmt.Errorf("Azure PR merge snapshot disagrees with the live source ref")
	}
	return facts, nil
}

// ReadPRFacts reads the exact recorded PR ID. A changed target branch is
// reported from the forge, while a changed identity is refused.
func (h *Host) ReadPRFacts(ctx context.Context, pr *scm.PR) (scm.PRFacts, error) {
	if pr == nil || h.org == "" || h.project == "" || h.repo == "" {
		return scm.PRFacts{}, fmt.Errorf("read Azure PR facts: missing PR or configured repository")
	}
	id, err := strconv.Atoi(strings.TrimSpace(pr.Number))
	if err != nil || id <= 0 || strconv.Itoa(id) != pr.Number {
		return scm.PRFacts{}, fmt.Errorf("read Azure PR facts: invalid PR number %q", pr.Number)
	}
	expectedURL := webPRURL(h.org, h.project, h.repo, "", pr.Number)
	if pr.URL != "" && pr.URL != expectedURL {
		return scm.PRFacts{}, fmt.Errorf("read Azure PR facts: recorded URL does not match PR identity")
	}
	raw, err := h.showPR(ctx, pr)
	if err != nil {
		return scm.PRFacts{}, err
	}
	facts, err := h.factsFromPR(*raw)
	if err != nil {
		return scm.PRFacts{}, err
	}
	if facts.PR.Number != pr.Number {
		return scm.PRFacts{}, fmt.Errorf("Azure PR show returned a different PR identity")
	}
	return h.bindLiveHead(ctx, facts, make(map[string]string))
}

// FindOpenPRFacts visits every Azure CLI page. The CLI's source-branch filter
// does not identify a fork; each candidate is validated before source matching.
func (h *Host) FindOpenPRFacts(ctx context.Context, sourceRepository, sourceBranch string) ([]scm.PRFacts, error) {
	if h.org == "" || h.project == "" || h.repo == "" || sourceBranch == "" || sourceBranch != strings.TrimSpace(sourceBranch) {
		return nil, fmt.Errorf("discover Azure PR facts: missing target or source identity")
	}
	_, org, project, repo, err := sourceRepositoryURL(sourceRepository)
	if err != nil || !strings.EqualFold(azureOrganizationName(org), azureOrganizationName(h.org)) {
		return nil, fmt.Errorf("discover Azure PR facts: invalid or foreign source repository")
	}
	wanted := webPRURL(h.org, project, repo, "", "")
	var facts []scm.PRFacts
	seen := make(map[string]bool)
	liveHeads := make(map[string]string)
	for skip := 0; ; skip += prFactsPageSize {
		args := []string{"repos", "pr", "list", "--source-branch", sourceBranch, "--status", "active", "--top", strconv.Itoa(prFactsPageSize), "--skip", strconv.Itoa(skip)}
		args = append(args, h.scopeArgs()...)
		args = append(args, "--output", "json")
		out, err := outputJSON(h.cmd(ctx, "az", args...))
		if err != nil {
			return nil, fmt.Errorf("az repos pr list page %d: %w", skip/prFactsPageSize, err)
		}
		var page []azPR
		if err := json.Unmarshal(bytes.TrimSpace(out), &page); err != nil || page == nil {
			return nil, fmt.Errorf("az repos pr list page %d: invalid array: %v", skip/prFactsPageSize, err)
		}
		if len(page) > prFactsPageSize {
			return nil, fmt.Errorf("az repos pr list returned more entries than requested")
		}
		for i, raw := range page {
			item, err := h.factsFromPR(raw)
			if err != nil {
				return nil, fmt.Errorf("az repos pr list page %d entry %d: %w", skip/prFactsPageSize, i, err)
			}
			if item.State != scm.PRStateOpen || item.SourceBranch != sourceBranch {
				return nil, fmt.Errorf("az repos pr list returned a non-open or mismatched source branch")
			}
			item, err = h.bindLiveHead(ctx, item, liveHeads)
			if err != nil {
				return nil, fmt.Errorf("az repos pr list page %d entry %d: %w", skip/prFactsPageSize, i, err)
			}
			if seen[item.PR.Number] {
				return nil, fmt.Errorf("az repos pr list returned duplicate PR %s", item.PR.Number)
			}
			seen[item.PR.Number] = true
			if scm.SameSourceRepository(scm.ProviderAzureDevOps, item.SourceRepository, wanted) {
				facts = append(facts, item)
			}
		}
		if len(page) < prFactsPageSize {
			return facts, nil
		}
	}
}

package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GitLab's REST merge request response exposes the diff head SHA and source
// project ID together. The source path needs a separate project read; glab mr
// list/view JSON is insufficient because a fork can use the same branch name.
type gitlabMRFactsWire struct {
	MergeCommitSHA  string `json:"merge_commit_sha"`
	SquashCommitSHA string `json:"squash_commit_sha"`
	IID             int    `json:"iid"`
	WebURL          string `json:"web_url"`
	State           string `json:"state"`
	SourceProjectID int    `json:"source_project_id"`
	TargetProjectID int    `json:"target_project_id"`
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	SHA             string `json:"sha"`
}

type gitlabProjectFactsWire struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

func (h *Host) factsAPI(ctx context.Context, args ...string) ([]byte, error) {
	command := []string{"api"}
	if h.host != "" {
		command = append(command, "--hostname", h.host)
	}
	command = append(command, "--method", "GET")
	command = append(command, args...)
	out, err := h.cmd(ctx, "glab", command...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab API PR facts: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

func (h *Host) factsProjectPath() (string, error) {
	path := strings.Trim(h.projectPath, "/")
	if !validGitlabProjectPath(path) || path != h.projectPath {
		return "", errors.New("GitLab target project path is unknown or invalid")
	}
	return path, nil
}

func validGitlabProjectPath(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part || strings.ContainsAny(part, "?#%\\") {
			return false
		}
	}
	return true
}

func isGitlabFullSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func (h *Host) projectForFacts(ctx context.Context, id int, cache map[int]string) (string, error) {
	if path, ok := cache[id]; ok {
		return path, nil
	}
	out, err := h.factsAPI(ctx, "projects/"+strconv.Itoa(id))
	if err != nil {
		return "", err
	}
	var project gitlabProjectFactsWire
	if err := json.Unmarshal(out, &project); err != nil {
		return "", fmt.Errorf("decode GitLab source project: %w", err)
	}
	if project.ID != id || !validGitlabProjectPath(project.PathWithNamespace) {
		return "", fmt.Errorf("GitLab source project %d has incomplete or inconsistent identity", id)
	}
	cache[id] = project.PathWithNamespace
	return project.PathWithNamespace, nil
}

func (h *Host) factsFromMR(ctx context.Context, wire gitlabMRFactsWire, cache map[int]string) (scm.PRFacts, error) {
	if wire.IID <= 0 || wire.SourceProjectID <= 0 || wire.TargetProjectID <= 0 ||
		strings.TrimSpace(wire.SourceBranch) == "" || strings.TrimSpace(wire.TargetBranch) == "" ||
		wire.SourceBranch != strings.TrimSpace(wire.SourceBranch) || wire.TargetBranch != strings.TrimSpace(wire.TargetBranch) ||
		!isGitlabFullSHA(wire.SHA) {
		return scm.PRFacts{}, errors.New("GitLab merge request has incomplete source, head, or target facts")
	}
	number, err := parseMergeRequestURL(wire.WebURL, h.host, h.projectPath)
	if err != nil || number != wire.IID || wire.WebURL != strings.TrimSpace(wire.WebURL) {
		return scm.PRFacts{}, fmt.Errorf("GitLab merge request has inconsistent URL and IID: %v", err)
	}
	var state scm.PRState
	switch wire.State {
	case "opened":
		state = scm.PRStateOpen
	case "merged":
		state = scm.PRStateMerged
	case "closed":
		state = scm.PRStateClosed
	default:
		return scm.PRFacts{}, fmt.Errorf("GitLab merge request %d has unsupported state %q", wire.IID, wire.State)
	}
	mergeSHA := wire.MergeCommitSHA
	if state == scm.PRStateMerged {
		if mergeSHA != "" && !isGitlabFullSHA(mergeSHA) ||
			wire.SquashCommitSHA != "" && !isGitlabFullSHA(wire.SquashCommitSHA) {
			return scm.PRFacts{}, errors.New("merged GitLab merge request has a malformed merge or squash commit SHA")
		}
		// GitLab's MergeRequest#merged_commit_sha uses merge_commit_sha,
		// squash_commit_sha, then diff_head_sha. The REST entity exposes that
		// diff head as sha. A fast-forward creates no separate merge commit.
		// Sources: app/models/merge_request.rb and
		// lib/api/entities/merge_request_basic.rb in gitlab-org/gitlab.
		if mergeSHA == "" {
			mergeSHA = wire.SquashCommitSHA
		}
		if mergeSHA == "" {
			mergeSHA = wire.SHA
		}
	}
	project, err := h.projectForFacts(ctx, wire.SourceProjectID, cache)
	if err != nil {
		return scm.PRFacts{}, err
	}
	target, err := h.projectForFacts(ctx, wire.TargetProjectID, cache)
	if err != nil {
		return scm.PRFacts{}, err
	}
	if !strings.EqualFold(target, h.projectPath) {
		return scm.PRFacts{}, errors.New("GitLab merge request target project identity disagrees with configured project")
	}
	pr := scm.PR{Number: strconv.Itoa(wire.IID), URL: wire.WebURL, HeadSHA: wire.SHA, BaseBranch: wire.TargetBranch}
	return scm.PRFacts{PR: pr, State: state, SourceRepository: project, SourceBranch: wire.SourceBranch, HeadSHA: wire.SHA, BaseBranch: wire.TargetBranch, MergeCommitSHA: mergeSHA}, nil
}

// ReadPRFacts reads a recorded MR by its target project and IID. A URL-only
// identity is accepted because older glab list output can omit the IID.
func (h *Host) ReadPRFacts(ctx context.Context, pr *scm.PR) (scm.PRFacts, error) {
	project, err := h.factsProjectPath()
	if err != nil {
		return scm.PRFacts{}, err
	}
	if pr == nil {
		return scm.PRFacts{}, errors.New("read GitLab MR facts: missing identity")
	}
	number := 0
	if pr.Number != "" {
		number, err = strconv.Atoi(pr.Number)
		if err != nil || number <= 0 || pr.Number != strconv.Itoa(number) {
			return scm.PRFacts{}, fmt.Errorf("read GitLab MR facts: invalid number %q", pr.Number)
		}
	}
	if pr.URL != "" {
		fromURL, urlErr := parseMergeRequestURL(pr.URL, h.host, project)
		if urlErr != nil || (number != 0 && number != fromURL) || pr.URL != strings.TrimSpace(pr.URL) {
			return scm.PRFacts{}, fmt.Errorf("read GitLab MR facts: invalid or conflicting URL: %v", urlErr)
		}
		number = fromURL
	}
	if number == 0 {
		return scm.PRFacts{}, errors.New("read GitLab MR facts: missing number and URL")
	}
	out, err := h.factsAPI(ctx, "projects/"+url.PathEscape(project)+"/merge_requests/"+strconv.Itoa(number))
	if err != nil {
		return scm.PRFacts{}, err
	}
	var wire gitlabMRFactsWire
	if err := json.Unmarshal(out, &wire); err != nil {
		return scm.PRFacts{}, fmt.Errorf("decode GitLab MR facts: %w", err)
	}
	facts, err := h.factsFromMR(ctx, wire, make(map[int]string))
	if err != nil {
		return scm.PRFacts{}, err
	}
	if facts.PR.Number != strconv.Itoa(number) || (pr.URL != "" && facts.PR.URL != pr.URL) {
		return scm.PRFacts{}, errors.New("GitLab MR facts do not match recorded identity")
	}
	return facts, nil
}

// FindOpenPRFacts reads every open MR for the branch. glab api --paginate may
// emit either one array or consecutive page arrays depending on CLI version.
// Every document must decode; a corrupt later page cannot yield a partial list.
func (h *Host) FindOpenPRFacts(ctx context.Context, sourceRepository, sourceBranch string) ([]scm.PRFacts, error) {
	project, err := h.factsProjectPath()
	if err != nil {
		return nil, err
	}
	if !validGitlabProjectPath(sourceRepository) || sourceBranch == "" || sourceBranch != strings.TrimSpace(sourceBranch) {
		return nil, errors.New("discover GitLab MR facts: invalid source identity")
	}
	out, err := h.factsAPI(ctx, "projects/"+url.PathEscape(project)+"/merge_requests",
		"-f", "state=opened", "-f", "source_branch="+sourceBranch, "-f", "per_page=100", "--paginate")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	cache := make(map[int]string)
	seen := make(map[string]bool)
	seenNumbers := make(map[string]bool)
	var facts []scm.PRFacts
	pages := 0
	for {
		var page []gitlabMRFactsWire
		if err := dec.Decode(&page); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode complete GitLab MR list: %w", err)
		}
		pages++
		if page == nil {
			return nil, errors.New("GitLab MR list contains a null page")
		}
		for _, candidate := range page {
			item, err := h.factsFromMR(ctx, candidate, cache)
			if err != nil {
				return nil, err
			}
			if item.State != scm.PRStateOpen || item.SourceBranch != sourceBranch {
				return nil, errors.New("GitLab MR list returned a non-open or mismatched branch")
			}
			if seen[item.PR.URL] || seenNumbers[item.PR.Number] {
				return nil, fmt.Errorf("GitLab MR list returned duplicate %s", item.PR.Number)
			}
			seen[item.PR.URL] = true
			seenNumbers[item.PR.Number] = true
			if scm.SameSourceRepository(scm.ProviderGitLab, item.SourceRepository, sourceRepository) {
				facts = append(facts, item)
			}
		}
	}
	if pages == 0 {
		return nil, errors.New("decode complete GitLab MR list: no JSON pages")
	}
	return facts, nil
}

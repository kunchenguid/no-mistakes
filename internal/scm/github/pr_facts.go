package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type githubPRFactsWire struct {
	MergeCommitSHA string `json:"merge_commit_sha"`
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (h *Host) prFactsFromWire(w githubPRFactsWire) (scm.PRFacts, error) {
	if w.Number <= 0 || strings.TrimSpace(w.HTMLURL) == "" {
		return scm.PRFacts{}, fmt.Errorf("GitHub PR facts lack a number or URL")
	}
	gotNumber, err := parsePullRequestURL(w.HTMLURL, h.host, h.repoSlug())
	if err != nil || gotNumber != w.Number {
		return scm.PRFacts{}, fmt.Errorf("GitHub PR facts have inconsistent identity: %v", err)
	}
	state := scm.PRStateOpen
	switch w.State {
	case "open":
		if w.Merged {
			return scm.PRFacts{}, fmt.Errorf("open GitHub PR %d reports merged", w.Number)
		}
	case "closed":
		state = scm.PRStateClosed
		if w.Merged {
			state = scm.PRStateMerged
		}
	default:
		return scm.PRFacts{}, fmt.Errorf("GitHub PR %d has invalid state %q", w.Number, w.State)
	}
	if w.Head.Repo == nil || strings.TrimSpace(w.Head.Repo.FullName) == "" ||
		strings.TrimSpace(w.Head.Ref) == "" || strings.TrimSpace(w.Base.Ref) == "" ||
		!isFullCommitSHA(w.Head.SHA) {
		return scm.PRFacts{}, fmt.Errorf("GitHub PR %d has incomplete source, head, or target facts", w.Number)
	}
	if state == scm.PRStateMerged && !isFullCommitSHA(w.MergeCommitSHA) {
		return scm.PRFacts{}, fmt.Errorf("merged GitHub PR %d lacks a full merge commit SHA", w.Number)
	}
	number := strconv.Itoa(w.Number)
	return scm.PRFacts{
		PR:               scm.PR{Number: number, URL: w.HTMLURL, HeadSHA: w.Head.SHA, BaseBranch: w.Base.Ref},
		State:            state,
		MergeCommitSHA:   w.MergeCommitSHA,
		SourceRepository: strings.TrimSpace(w.Head.Repo.FullName),
		SourceBranch:     strings.TrimSpace(w.Head.Ref),
		HeadSHA:          w.Head.SHA,
		BaseBranch:       strings.TrimSpace(w.Base.Ref),
	}, nil
}

func isFullCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func (h *Host) prFactsAPI(ctx context.Context, args ...string) ([]byte, error) {
	command := []string{"api"}
	if h.host != "" {
		command = append(command, "--hostname", h.host)
	}
	command = append(command, args...)
	out, err := h.cmd(ctx, "gh", command...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api pull request facts: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

// ReadPRFacts reads the exact recorded PR, without substituting a branch-list
// hit whose head or target may differ.
func (h *Host) ReadPRFacts(ctx context.Context, pr *scm.PR) (scm.PRFacts, error) {
	if pr == nil {
		return scm.PRFacts{}, fmt.Errorf("read GitHub PR facts: missing identity")
	}
	number, err := strconv.Atoi(strings.TrimSpace(pr.Number))
	if err != nil || number <= 0 {
		return scm.PRFacts{}, fmt.Errorf("read GitHub PR facts: invalid number %q", pr.Number)
	}
	repo := h.repoSlug()
	if repo == "" {
		return scm.PRFacts{}, fmt.Errorf("read GitHub PR facts: repository is unknown")
	}
	out, err := h.prFactsAPI(ctx, "--method", "GET", "repos/"+repo+"/pulls/"+strconv.Itoa(number))
	if err != nil {
		return scm.PRFacts{}, err
	}
	var wire githubPRFactsWire
	if err := json.Unmarshal(out, &wire); err != nil {
		return scm.PRFacts{}, fmt.Errorf("decode GitHub PR facts: %w", err)
	}
	facts, err := h.prFactsFromWire(wire)
	if err != nil {
		return scm.PRFacts{}, err
	}
	if facts.PR.Number != pr.Number || (strings.TrimSpace(pr.URL) != "" && facts.PR.URL != strings.TrimSpace(pr.URL)) {
		return scm.PRFacts{}, fmt.Errorf("GitHub PR facts do not match recorded PR identity")
	}
	return facts, nil
}

// FindOpenPRFacts returns all open candidates for the exact source. A complete
// paginated response is required so a first-list-hit cannot hide ambiguity.
func (h *Host) FindOpenPRFacts(ctx context.Context, sourceRepository, sourceBranch string) ([]scm.PRFacts, error) {
	repo := h.repoSlug()
	if repo == "" {
		return nil, fmt.Errorf("discover GitHub PR facts: target repository is unknown")
	}
	parts := strings.Split(sourceRepository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.TrimSpace(sourceBranch) == "" {
		return nil, fmt.Errorf("discover GitHub PR facts: invalid source identity")
	}
	out, err := h.prFactsAPI(ctx, "--method", "GET", "repos/"+repo+"/pulls",
		"-f", "state=open", "-f", "head="+parts[0]+":"+sourceBranch,
		"-f", "per_page=100", "--paginate", "--slurp")
	if err != nil {
		return nil, err
	}
	var pages [][]githubPRFactsWire
	if err := json.Unmarshal(out, &pages); err != nil || pages == nil {
		return nil, fmt.Errorf("decode complete GitHub PR list: %v", err)
	}
	var facts []scm.PRFacts
	seen := make(map[string]bool)
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("GitHub PR list contains a null page")
		}
		for _, candidate := range page {
			item, err := h.prFactsFromWire(candidate)
			if err != nil {
				return nil, err
			}
			if item.State != scm.PRStateOpen || item.SourceBranch != sourceBranch {
				return nil, fmt.Errorf("GitHub PR list returned a non-open or mismatched branch")
			}
			if !scm.SameSourceRepository(scm.ProviderGitHub, item.SourceRepository, sourceRepository) {
				continue
			}
			if seen[item.PR.URL] {
				return nil, fmt.Errorf("GitHub PR list returned duplicate %s", item.PR.URL)
			}
			seen[item.PR.URL] = true
			facts = append(facts, item)
		}
	}
	return facts, nil
}

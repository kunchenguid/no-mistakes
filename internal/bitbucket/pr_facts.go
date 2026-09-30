package bitbucket

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) factsFromWire(w bitbucketPullRequest) (scm.PRFacts, error) {
	wantURL := prURL(h.repo, w.ID, "")
	if w.ID <= 0 || wantURL == "" || strings.TrimSpace(w.Links.HTML.Href) != wantURL {
		return scm.PRFacts{}, fmt.Errorf("Bitbucket PR facts have inconsistent repository or PR identity")
	}
	state := normalizePRState(w.State)
	if state != scm.PRStateOpen && state != scm.PRStateClosed && state != scm.PRStateMerged {
		return scm.PRFacts{}, fmt.Errorf("Bitbucket PR %d has invalid state %q", w.ID, w.State)
	}
	sourceRepo := strings.TrimSpace(w.Source.Repository.FullName)
	if sourceRepo == "" || strings.TrimSpace(w.Source.Branch.Name) == "" ||
		strings.TrimSpace(w.Destination.Branch.Name) == "" || !fullBitbucketHash(w.Source.Commit.Hash) {
		return scm.PRFacts{}, fmt.Errorf("Bitbucket PR %d has incomplete source, head, or target facts", w.ID)
	}
	return scm.PRFacts{
		PR:               scm.PR{Number: strconv.Itoa(w.ID), URL: wantURL, HeadSHA: w.Source.Commit.Hash, BaseBranch: w.Destination.Branch.Name},
		State:            state,
		SourceRepository: sourceRepo,
		SourceBranch:     w.Source.Branch.Name,
		HeadSHA:          w.Source.Commit.Hash,
		BaseBranch:       w.Destination.Branch.Name,
	}, nil
}

func fullBitbucketHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

func (h *Host) ReadPRFacts(ctx context.Context, identity *scm.PR) (scm.PRFacts, error) {
	if h.client == nil || identity == nil {
		return scm.PRFacts{}, fmt.Errorf("read Bitbucket PR facts: missing client or PR identity")
	}
	id, err := strconv.Atoi(identity.Number)
	if err != nil || id <= 0 {
		return scm.PRFacts{}, fmt.Errorf("read Bitbucket PR facts: invalid number %q", identity.Number)
	}
	if identity.URL != "" && identity.URL != prURL(h.repo, id, "") {
		return scm.PRFacts{}, fmt.Errorf("read Bitbucket PR facts: recorded URL does not match configured repository")
	}
	var wire bitbucketPullRequest
	if err := h.client.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/%d", repoPRPath(h.repo), id), nil, nil, &wire); err != nil {
		return scm.PRFacts{}, err
	}
	if wire.ID != id {
		return scm.PRFacts{}, fmt.Errorf("Bitbucket PR facts returned id %d for requested %d", wire.ID, id)
	}
	return h.factsFromWire(wire)
}

// GetPRBaseBranch supplies the live target for callers that discover a PR
// through the older Host interface, whose PR value has no Bitbucket base.
func (h *Host) GetPRBaseBranch(ctx context.Context, pr *scm.PR) (string, error) {
	facts, err := h.ReadPRFacts(ctx, pr)
	if err != nil {
		return "", err
	}
	return facts.BaseBranch, nil
}

func (h *Host) FindOpenPRFacts(ctx context.Context, sourceRepository, sourceBranch string) ([]scm.PRFacts, error) {
	if h.client == nil {
		return nil, fmt.Errorf("discover Bitbucket PR facts: client unavailable")
	}
	sourceRepository = strings.TrimSpace(sourceRepository)
	if sourceRepository == "" || strings.TrimSpace(sourceBranch) == "" {
		return nil, fmt.Errorf("Bitbucket PR source identity is incomplete")
	}
	query := url.Values{}
	query.Set("q", fmt.Sprintf(`source.branch.name=%q AND source.repository.full_name=%q AND state=%q`, sourceBranch, sourceRepository, "OPEN"))
	query.Set("pagelen", "100")
	next := repoPRPath(h.repo) + "?" + query.Encode()
	seenPages := map[string]bool{}
	seenPRs := map[int]bool{}
	facts := []scm.PRFacts{}
	for next != "" {
		if seenPages[next] {
			return nil, fmt.Errorf("Bitbucket PR listing repeated a page")
		}
		seenPages[next] = true
		var response struct {
			Values []bitbucketPullRequest `json:"values"`
			Next   string                 `json:"next"`
		}
		if err := h.client.doJSONPathOrURL(ctx, http.MethodGet, next, nil, &response); err != nil {
			return nil, err
		}
		if response.Values == nil {
			return nil, fmt.Errorf("Bitbucket PR listing lacks a values array")
		}
		for _, candidate := range response.Values {
			item, err := h.factsFromWire(candidate)
			if err != nil {
				return nil, err
			}
			if item.State != scm.PRStateOpen || !scm.SameSourceRepository(scm.ProviderBitbucket, item.SourceRepository, sourceRepository) || item.SourceBranch != sourceBranch {
				return nil, fmt.Errorf("Bitbucket PR listing returned a mismatched source identity")
			}
			if seenPRs[candidate.ID] {
				return nil, fmt.Errorf("Bitbucket PR listing repeated PR %d", candidate.ID)
			}
			seenPRs[candidate.ID] = true
			facts = append(facts, item)
		}
		if response.Next == "" {
			break
		}
		validated, err := h.client.validatePaginationURL(response.Next)
		if err != nil {
			return nil, err
		}
		parsed, err := url.Parse(validated)
		if err != nil || parsed.Path != repoPRPath(h.repo) {
			return nil, fmt.Errorf("Bitbucket PR pagination changed repository path")
		}
		next = validated
	}
	return facts, nil
}

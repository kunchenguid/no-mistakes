package forgejo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type rawPullFacts struct {
	MergeCommitSHA string `json:"merge_commit_sha"`
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Merged         *bool  `json:"merged"`
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

type rawPullListItem struct {
	Number int `json:"number"`
}

func (h *Host) factsFromRaw(pull rawPullFacts) (scm.PRFacts, error) {
	if pull.Number <= 0 || pull.Merged == nil || pull.Head.Repo == nil ||
		strings.TrimSpace(pull.Head.Repo.FullName) == "" ||
		strings.TrimSpace(pull.Head.Ref) == "" ||
		strings.TrimSpace(pull.Base.Ref) == "" ||
		!isFullForgejoSHA(pull.Head.SHA) {
		return scm.PRFacts{}, errors.New("Forgejo raw PR facts lack source, head, or target identity")
	}
	number := strconv.Itoa(pull.Number)
	wantURL := strings.TrimRight(h.baseURL, "/") + "/" + h.repository + "/pulls/" + number
	if pull.HTMLURL != wantURL {
		return scm.PRFacts{}, fmt.Errorf("Forgejo raw PR URL %q does not match %q", pull.HTMLURL, wantURL)
	}
	state := scm.PRStateOpen
	switch pull.State {
	case "open":
		if *pull.Merged {
			return scm.PRFacts{}, errors.New("open Forgejo PR reports merged")
		}
	case "closed":
		state = scm.PRStateClosed
		if *pull.Merged {
			state = scm.PRStateMerged
		}
	default:
		return scm.PRFacts{}, fmt.Errorf("Forgejo PR has invalid state %q", pull.State)
	}
	if state == scm.PRStateMerged && !isFullForgejoSHA(pull.MergeCommitSHA) {
		return scm.PRFacts{}, errors.New("merged Forgejo PR lacks a full merge commit SHA")
	}
	pr := scm.PR{Number: number, URL: wantURL, HeadSHA: pull.Head.SHA, BaseBranch: pull.Base.Ref}
	return scm.PRFacts{
		PR:               pr,
		State:            state,
		MergeCommitSHA:   pull.MergeCommitSHA,
		SourceRepository: pull.Head.Repo.FullName,
		SourceBranch:     pull.Head.Ref,
		HeadSHA:          pull.Head.SHA,
		BaseBranch:       pull.Base.Ref,
	}, nil
}

func isFullForgejoSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func (h *Host) ReadPRFacts(ctx context.Context, identity *scm.PR) (scm.PRFacts, error) {
	number, err := h.validateInputPR(identity)
	if err != nil {
		return scm.PRFacts{}, err
	}
	var response struct {
		Status int           `json:"status"`
		Data   *rawPullFacts `json:"data"`
	}
	if err := h.runJSON(ctx, "api", []string{"GET", "repos/" + h.repository + "/pulls/" + number}, &response); err != nil {
		return scm.PRFacts{}, err
	}
	if response.Status != 200 || response.Data == nil {
		return scm.PRFacts{}, errors.New("Forgejo raw PR facts response is incomplete")
	}
	if err := h.validateOutputPRNumber(number, response.Data.Number); err != nil {
		return scm.PRFacts{}, err
	}
	facts, err := h.factsFromRaw(*response.Data)
	if err != nil {
		return scm.PRFacts{}, err
	}
	if identity.URL != "" && identity.URL != facts.PR.URL {
		return scm.PRFacts{}, errors.New("Forgejo raw PR identity differs from recorded URL")
	}
	return facts, nil
}

func (h *Host) FindOpenPRFacts(ctx context.Context, sourceRepository, sourceBranch string) ([]scm.PRFacts, error) {
	if strings.TrimSpace(sourceRepository) == "" || strings.TrimSpace(sourceBranch) == "" ||
		sourceRepository != strings.TrimSpace(sourceRepository) || sourceBranch != strings.TrimSpace(sourceBranch) {
		return nil, fmt.Errorf("Forgejo PR source identity is incomplete")
	}
	const pageSize = 50
	seen := make(map[string]bool)
	var matches []scm.PRFacts
	for page := 1; page <= 10000; page++ {
		endpoint := fmt.Sprintf("repos/%s/pulls?state=open&sort=oldest&limit=%d&page=%d", h.repository, pageSize, page)
		var response struct {
			Status int               `json:"status"`
			Data   []rawPullListItem `json:"data"`
		}
		if err := h.runJSON(ctx, "api", []string{"GET", endpoint}, &response); err != nil {
			return nil, fmt.Errorf("read complete Forgejo PR list page %d: %w", page, err)
		}
		if response.Status != 200 || response.Data == nil {
			return nil, fmt.Errorf("read complete Forgejo PR list page %d: invalid array", page)
		}
		candidates := response.Data
		if len(candidates) > pageSize {
			return nil, fmt.Errorf("Forgejo PR list page %d exceeds requested size", page)
		}
		if len(candidates) == 0 {
			if len(matches) > 1 {
				return nil, fmt.Errorf("ambiguous Forgejo PR search: %d exact source candidates", len(matches))
			}
			return matches, nil
		}
		for i, candidate := range candidates {
			if candidate.Number <= 0 {
				return nil, fmt.Errorf("Forgejo PR list page %d entry %d has invalid number", page, i)
			}
			number := strconv.Itoa(candidate.Number)
			if seen[number] {
				return nil, fmt.Errorf("Forgejo PR list returned duplicate %s", number)
			}
			seen[number] = true
			facts, err := h.ReadPRFacts(ctx, &scm.PR{Number: number, URL: h.canonicalPRURL(candidate.Number)})
			if err != nil {
				return nil, fmt.Errorf("read Forgejo PR list page %d entry %d: %w", page, i, err)
			}
			if facts.State != scm.PRStateOpen {
				return nil, fmt.Errorf("Forgejo PR list page %d entry %d returned a non-open PR", page, i)
			}
			if scm.SameSourceRepository(scm.ProviderForgejo, facts.SourceRepository, sourceRepository) && facts.SourceBranch == sourceBranch {
				matches = append(matches, facts)
			}
		}
	}
	return nil, errors.New("Forgejo PR list exceeded pagination limit")
}

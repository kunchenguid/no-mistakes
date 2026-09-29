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
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  *bool  `json:"merged"`
	Head    struct {
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
	if *pull.Merged {
		state = scm.PRStateMerged
	} else if pull.State == "closed" {
		state = scm.PRStateClosed
	} else if pull.State != "open" {
		return scm.PRFacts{}, fmt.Errorf("Forgejo PR has invalid state %q", pull.State)
	}
	if pull.State == "open" && *pull.Merged {
		return scm.PRFacts{}, errors.New("open Forgejo PR reports merged")
	}
	pr := scm.PR{Number: number, URL: wantURL, HeadSHA: pull.Head.SHA, BaseBranch: pull.Base.Ref}
	return scm.PRFacts{
		PR:               pr,
		State:            state,
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
	if sourceRepository != h.repository || strings.TrimSpace(sourceBranch) == "" {
		return nil, fmt.Errorf("Forgejo PR source identity does not match configured repository")
	}
	var response struct {
		Found       bool         `json:"found"`
		PullRequest *pullRequest `json:"pull_request"`
		SearchInfo  struct {
			Complete bool `json:"complete"`
			Pages    int  `json:"pages"`
			Fetched  int  `json:"fetched"`
			Total    *int `json:"total"`
		} `json:"search_info"`
	}
	if err := h.runJSON(ctx, "pr find", []string{"--repo", h.repository, "--head", sourceBranch, "--state", "open"}, &response); err != nil {
		return nil, err
	}
	info := response.SearchInfo
	if !info.Complete || info.Pages <= 0 || info.Total == nil || info.Fetched < 0 || *info.Total < info.Fetched {
		return nil, errors.New("Forgejo PR search is incomplete or inconsistent")
	}
	if *info.Total > 1 {
		return nil, fmt.Errorf("ambiguous Forgejo PR search: %d open candidates", *info.Total)
	}
	if !response.Found {
		if *info.Total != 0 || info.Fetched != 0 || response.PullRequest != nil {
			return nil, errors.New("Forgejo PR search reported contradictory absence")
		}
		return []scm.PRFacts{}, nil
	}
	if *info.Total != 1 || info.Fetched != 1 || response.PullRequest == nil {
		return nil, errors.New("Forgejo PR search reported inconsistent candidate count")
	}
	facts, err := h.ReadPRFacts(ctx, &scm.PR{Number: strconv.Itoa(response.PullRequest.Number), URL: response.PullRequest.URL})
	if err != nil {
		return nil, err
	}
	if facts.State != scm.PRStateOpen || facts.SourceBranch != sourceBranch ||
		facts.HeadSHA != response.PullRequest.HeadSHA || facts.BaseBranch != response.PullRequest.Base {
		return nil, errors.New("Forgejo PR search returned a non-open or mismatched branch")
	}
	if facts.SourceRepository != sourceRepository {
		return []scm.PRFacts{}, nil
	}
	return []scm.PRFacts{facts}, nil
}

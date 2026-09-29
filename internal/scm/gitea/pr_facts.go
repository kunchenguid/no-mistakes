package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// tea's formatted pulls list and view have different field types and neither
// carries the source repository. PR facts therefore use the authenticated raw
// API, where both a single pull and each list entry have this shape.
type giteaPRFactsWire struct {
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

func (h *Host) factsRepository() (string, string, error) {
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok || strings.Contains(repo, "/") || owner != strings.TrimSpace(owner) || repo != strings.TrimSpace(repo) {
		return "", "", fmt.Errorf("Gitea PR facts: invalid target repository %q", h.repoSlug)
	}
	return owner, repo, nil
}

func validGiteaSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (h *Host) factsFromWire(w giteaPRFactsWire, owner, repo string) (scm.PRFacts, error) {
	if w.Number <= 0 || w.Merged == nil || w.Head.Repo == nil ||
		strings.TrimSpace(w.Head.Repo.FullName) == "" || strings.TrimSpace(w.Head.Ref) == "" ||
		strings.TrimSpace(w.Base.Ref) == "" || !validGiteaSHA(w.Head.SHA) {
		return scm.PRFacts{}, fmt.Errorf("Gitea PR facts are incomplete or malformed")
	}
	if w.Head.Repo.FullName != strings.TrimSpace(w.Head.Repo.FullName) ||
		w.Head.Ref != strings.TrimSpace(w.Head.Ref) || w.Base.Ref != strings.TrimSpace(w.Base.Ref) {
		return scm.PRFacts{}, fmt.Errorf("Gitea PR facts contain noncanonical source or target")
	}
	parts := strings.Split(w.Head.Repo.FullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return scm.PRFacts{}, fmt.Errorf("Gitea PR facts have invalid source repository")
	}
	parsed, err := url.Parse(w.HTMLURL)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!strings.HasSuffix(parsed.Path, fmt.Sprintf("/%s/%s/pulls/%d", owner, repo, w.Number)) ||
		(h.host != "" && !strings.EqualFold(parsed.Hostname(), h.host)) {
		return scm.PRFacts{}, fmt.Errorf("Gitea PR facts have inconsistent URL or number")
	}
	state := scm.PRStateOpen
	switch w.State {
	case "open":
		if *w.Merged {
			return scm.PRFacts{}, fmt.Errorf("open Gitea PR %d reports merged", w.Number)
		}
	case "closed":
		state = scm.PRStateClosed
		if *w.Merged {
			state = scm.PRStateMerged
		}
	default:
		return scm.PRFacts{}, fmt.Errorf("Gitea PR %d has invalid state %q", w.Number, w.State)
	}
	number := strconv.Itoa(w.Number)
	return scm.PRFacts{
		PR:               scm.PR{Number: number, URL: w.HTMLURL, HeadSHA: w.Head.SHA, BaseBranch: w.Base.Ref},
		State:            state,
		SourceRepository: w.Head.Repo.FullName,
		SourceBranch:     w.Head.Ref,
		HeadSHA:          w.Head.SHA,
		BaseBranch:       w.Base.Ref,
	}, nil
}

func (h *Host) factsAPI(ctx context.Context, endpoint string) ([]byte, error) {
	// Every tea invocation needs --login: the daemon's bare gate has no remote
	// from which tea can infer the correct instance.
	if h.login == "" {
		return nil, fmt.Errorf("Gitea PR facts: tea login is not configured")
	}
	out, err := h.cmd(ctx, "tea", "api", "--login", h.login, endpoint).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tea api PR facts: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

func decodeFactsJSON(out []byte, dest any) error {
	decoder := json.NewDecoder(bytes.NewReader(out))
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON data")
		}
		return fmt.Errorf("trailing or malformed JSON data: %w", err)
	}
	return nil
}

// ReadPRFacts observes the recorded PR itself; a branch-list hit could refer
// to a different review object and cannot establish the recorded identity.
func (h *Host) ReadPRFacts(ctx context.Context, pr *scm.PR) (scm.PRFacts, error) {
	if pr == nil {
		return scm.PRFacts{}, fmt.Errorf("read Gitea PR facts: missing identity")
	}
	number, err := strconv.Atoi(pr.Number)
	if err != nil || number <= 0 || strconv.Itoa(number) != pr.Number {
		return scm.PRFacts{}, fmt.Errorf("read Gitea PR facts: invalid number %q", pr.Number)
	}
	owner, repo, err := h.factsRepository()
	if err != nil {
		return scm.PRFacts{}, err
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	out, err := h.factsAPI(ctx, endpoint)
	if err != nil {
		return scm.PRFacts{}, err
	}
	var wire giteaPRFactsWire
	if err := decodeFactsJSON(out, &wire); err != nil {
		return scm.PRFacts{}, fmt.Errorf("decode Gitea PR facts: %w", err)
	}
	facts, err := h.factsFromWire(wire, owner, repo)
	if err != nil {
		return scm.PRFacts{}, err
	}
	if facts.PR.Number != pr.Number || (pr.URL != "" && facts.PR.URL != pr.URL) {
		return scm.PRFacts{}, fmt.Errorf("Gitea PR facts do not match recorded PR identity")
	}
	return facts, nil
}

// FindOpenPRFacts walks every page until the API returns an empty page. A
// shorter-than-requested page is not proof of completion: the server may cap
// its page size below the requested limit. Sorting oldest keeps new PRs at the
// end of the sequence while the scan proceeds.
func (h *Host) FindOpenPRFacts(ctx context.Context, sourceRepository, sourceBranch string) ([]scm.PRFacts, error) {
	owner, repo, err := h.factsRepository()
	if err != nil {
		return nil, err
	}
	parts := strings.Split(sourceRepository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.TrimSpace(sourceBranch) == "" ||
		sourceRepository != strings.TrimSpace(sourceRepository) || sourceBranch != strings.TrimSpace(sourceBranch) {
		return nil, fmt.Errorf("discover Gitea PR facts: invalid source identity")
	}
	var facts []scm.PRFacts
	seen := make(map[string]bool)
	for page := 1; page <= 10000; page++ {
		endpoint := fmt.Sprintf("/repos/%s/%s/pulls?state=open&sort=oldest&limit=50&page=%d", owner, repo, page)
		out, err := h.factsAPI(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		var wire []giteaPRFactsWire
		if err := decodeFactsJSON(out, &wire); err != nil || wire == nil {
			return nil, fmt.Errorf("decode complete Gitea PR list page %d: %v", page, err)
		}
		if len(wire) == 0 {
			return facts, nil
		}
		for _, candidate := range wire {
			item, err := h.factsFromWire(candidate, owner, repo)
			if err != nil {
				return nil, err
			}
			if item.State != scm.PRStateOpen {
				return nil, fmt.Errorf("Gitea open PR list returned a non-open PR")
			}
			if seen[item.PR.Number] {
				return nil, fmt.Errorf("Gitea open PR list returned duplicate PR %s", item.PR.Number)
			}
			seen[item.PR.Number] = true
			if strings.EqualFold(item.SourceRepository, sourceRepository) && item.SourceBranch == sourceBranch {
				facts = append(facts, item)
			}
		}
	}
	return nil, fmt.Errorf("Gitea open PR list exceeded pagination limit")
}

var _ scm.PRFactsReader = (*Host)(nil)

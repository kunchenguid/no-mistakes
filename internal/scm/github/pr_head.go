package github

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// VerifyPRHead reads by immutable PR identity, independently of FindPR and any
// create/update response. The REST object supplies both repository identities,
// lifecycle state and the exact head in one observation.
func (h *Host) VerifyPRHead(ctx context.Context, pr *scm.PR, branch, pushedHead string) error {
	if pr == nil || h.repoSlug() == "" || branch == "" || !completeObjectID(pushedHead) {
		return fmt.Errorf("verify PR head: incomplete expected identity or pushed head")
	}
	number, err := parsePullRequestURL(pr.URL, h.host, h.repoSlug())
	if err != nil {
		return fmt.Errorf("verify PR head: %w", err)
	}
	if pr.Number != strconv.Itoa(number) {
		return fmt.Errorf("verify PR head: PR number does not match URL")
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, fmt.Sprintf("repos/%s/pulls/%d", h.repoSlug(), number))
	out, detail, err := stdoutOnly(h.cmd(ctx, "gh", args...))
	if err != nil {
		return fmt.Errorf("verify PR head: gh api: %s: %w", detail, err)
	}
	type repository struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	var live struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
		State  string `json:"state"`
		Merged *bool  `json:"merged"`
		Base   struct {
			Repo repository `json:"repo"`
		} `json:"base"`
		Head struct {
			Ref  string     `json:"ref"`
			SHA  string     `json:"sha"`
			Repo repository `json:"repo"`
		} `json:"head"`
	}
	if err := json.Unmarshal(out, &live); err != nil {
		return fmt.Errorf("verify PR head: unreadable proof: %w", err)
	}
	liveNumber, err := parsePullRequestURL(live.URL, h.host, h.repoSlug())
	if err != nil || liveNumber != number || live.Number != number || !strings.EqualFold(live.Base.Repo.FullName, h.repoSlug()) {
		return fmt.Errorf("verify PR head: live review object does not match the configured repository and PR")
	}
	if live.State != "open" || live.Merged == nil || *live.Merged {
		return fmt.Errorf("verify PR head: review object is not proven open and unmerged")
	}
	source := h.repoSlug()
	if h.forkRepo != "" {
		source = h.forkRepo
	}
	if live.Head.Ref != branch || !strings.EqualFold(live.Head.Repo.FullName, source) || !strings.EqualFold(live.Head.Repo.Owner.Login, repoOwner(source)) {
		return fmt.Errorf("verify PR head: source repository, owner or branch does not match the push target")
	}
	if !completeObjectID(live.Head.SHA) {
		return fmt.Errorf("verify PR head: live head is missing or invalid")
	}
	if !strings.EqualFold(live.Head.SHA, pushedHead) {
		return fmt.Errorf("verify PR head: %w does not equal successfully pushed head %s", &scm.PRHeadChangedError{HeadSHA: live.Head.SHA}, pushedHead)
	}
	return nil
}

func completeObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

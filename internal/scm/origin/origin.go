// Package origin implements scm.Host backed by the Cursor Origin CLI
// (`origin`). Origin is Cursor's git forge (origin.cursor.com); it is not
// GitHub-compatible, so mapping it through `gh` would talk to the wrong API.
//
// Traps, verified against origin 2026.08.24 and live Origin JSON:
//
//   - `origin pr create` defaults to `--status draft`. no-mistakes' other
//     providers open ready PRs unless draft_pull_requests is set, so CreatePR
//     always passes `--status` explicitly (open or draft).
//   - `origin pr create` has no `--json` flag. CreatePR takes the PR URL the
//     CLI prints on create, and only without one re-lists by head branch and
//     picks the newest live PR.
//   - Origin treats drafts as status=draft, not open. FindPR lists
//     `--state open` and `--state draft` separately (closed PR history cannot
//     hide a live PR) and accepts open or draft so a draft opened by a previous
//     run is updated instead of duplicated.
//   - The daemon's detached bare-gate repo has no Origin remote, so every
//     invocation carries `--repo owner/repo` (the same reason gh/tea do).
//   - Git remotes are `https://origin.cursor.com/{owner}/{repo}.git`. The web
//     PR URL is `https://cursor.com/codebase/{owner}/{repo}/pull/{n}`. RepoSlug
//     strips the `/codebase/` prefix so both forms yield the same -R slug.
package origin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

const (
	listJSONFields  = "number,url,status,headRef,baseRef"
	viewJSONFields  = "number,url,status,title,description,headRef,baseRef,headSha,mergeability"
	checkJSONFields = "id,name,status,conclusion,detailsUrl,startedAt,completedAt"
)

var (
	_ scm.Host               = (*Host)(nil)
	_ scm.PRBaseRetargeter   = (*Host)(nil)
	_ scm.PRBaseBranchReader = (*Host)(nil)
	_ scm.PRContentReader    = (*Host)(nil)
)

// Host talks to Cursor Origin through the origin CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	repo         string // "owner/name" slug for --repo
	draft        bool   // open created PRs as Origin drafts (--status draft)
}

// New builds a Host. cliAvailable reports whether the origin binary is
// resolvable on the caller's PATH. repo is the "owner/name" slug passed via
// --repo so commands resolve the right repository from the daemon's
// non-repo working directory. draft opens created PRs as Origin drafts.
func New(cmd CmdFactory, cliAvailable func() bool, repo string, draft bool) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		repo:         strings.TrimSpace(repo),
		draft:        draft,
	}
}

// RepoSlug extracts the "owner/name" identifier from an Origin git remote
// (`https://origin.cursor.com/owner/name.git`) or web PR URL
// (`https://cursor.com/codebase/owner/name/pull/N`). The `/codebase/` prefix
// on web URLs is not part of the CLI's -R slug.
func RepoSlug(remoteURL string) string {
	parts := strings.Split(scm.RepoPath(remoteURL), "/")
	if len(parts) > 0 && strings.EqualFold(parts[0], "codebase") {
		parts = parts[1:]
	}
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func (h *Host) repoArgs() []string {
	if h.repo == "" {
		return nil
	}
	return []string{"--repo", h.repo}
}

func prSelector(pr *scm.PR) (string, error) {
	if pr != nil {
		if n := strings.TrimSpace(pr.Number); n != "" {
			return n, nil
		}
		if u := strings.TrimSpace(pr.URL); u != "" {
			return u, nil
		}
	}
	return "", errors.New("no PR number or URL known; refusing to run origin with a cwd-inferred branch")
}

func (h *Host) Provider() scm.Provider { return scm.ProviderOrigin }

func (h *Host) Capabilities() scm.Capabilities {
	// FailedCheckLogs is declined: origin pr checks has no log-body command.
	// MergedProof is declined: Origin reports mergedBy as an object on some
	// changes and a bare string on others, so merge metadata is not read;
	// GetPRState still reports MERGED from the change status.
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: false}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("origin CLI is not installed")
	}
	cmd := h.cmd(ctx, "origin", "auth", "status")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("origin auth status timed out: %w", ctx.Err())
		}
		if ctx.Err() != nil {
			return fmt.Errorf("origin auth status interrupted: %w", ctx.Err())
		}
		if isMissingExecutable(err) {
			return fmt.Errorf("origin CLI is not on PATH: %w", err)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("origin CLI is not authenticated: %s: %w", detail, err)
		}
		return fmt.Errorf("origin CLI is not authenticated: %w", err)
	}
	return nil
}

func isMissingExecutable(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errors.Is(execErr.Err, exec.ErrNotFound) || errors.Is(execErr.Err, fs.ErrNotExist)
	}
	return false
}

type originPR struct {
	Number       json.Number        `json:"number"`
	URL          string             `json:"url"`
	Status       string             `json:"status"`
	Title        string             `json:"title"`
	Description  *string            `json:"description"`
	HeadRef      string             `json:"headRef"`
	BaseRef      string             `json:"baseRef"`
	HeadSHA      string             `json:"headSha"`
	Mergeability originMergeability `json:"mergeability"`
}

// originMergeability mirrors the live `pr view --json mergeability` shape:
// the ruleset verdict is nested one level down under mergeability.mergeability,
// next to the top-level mergeable/hasMergeConflicts booleans.
type originMergeability struct {
	Mergeable         *bool    `json:"mergeable"`
	HasMergeConflicts *bool    `json:"hasMergeConflicts"`
	ConflictedPaths   []string `json:"conflictedPaths"`
	Mergeability      struct {
		Verdict string `json:"verdict"`
	} `json:"mergeability"`
}

func (p originPR) number() string {
	return strings.TrimSpace(p.Number.String())
}

func (p originPR) toPR() (*scm.PR, error) {
	num := p.number()
	url := strings.TrimSpace(p.URL)
	if num == "" || url == "" {
		return nil, errors.New("origin PR JSON missing number or url")
	}
	if n, err := strconv.Atoi(num); err != nil || n <= 0 {
		return nil, fmt.Errorf("origin PR JSON has invalid number %q", num)
	}
	parsed, err := parseOriginPRURL(url)
	if err != nil {
		return nil, fmt.Errorf("origin PR JSON has invalid url: %w", err)
	}
	if parsed != num {
		return nil, fmt.Errorf("origin PR JSON number %s does not match URL number %s", num, parsed)
	}
	return &scm.PR{
		Number:     num,
		URL:        url,
		HeadSHA:    strings.TrimSpace(p.HeadSHA),
		BaseBranch: strings.TrimSpace(p.BaseRef),
	}, nil
}

func originPRLive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "open", "draft":
		return true
	default:
		return false
	}
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	base = strings.TrimSpace(base)
	// Listing each live state separately keeps a branch's closed and merged
	// PR history from crowding a live PR out of the --limit window.
	var newest *scm.PR
	for _, state := range []string{"open", "draft"} {
		items, err := h.listPRs(ctx, branch, state)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if !originPRLive(item.Status) {
				continue
			}
			if strings.TrimSpace(item.HeadRef) != branch {
				continue
			}
			if base != "" && strings.TrimSpace(item.BaseRef) != base {
				continue
			}
			pr, err := item.toPR()
			if err != nil {
				return nil, err
			}
			if h.repo != "" {
				if slug := RepoSlug(pr.URL); slug != "" && !strings.EqualFold(slug, h.repo) {
					return nil, fmt.Errorf("origin PR URL repository %q does not match %q", slug, h.repo)
				}
			}
			// Several live PRs can share a head; the newest is the one a
			// create just made, so never rely on list order.
			if newest == nil || prNumber(pr) > prNumber(newest) {
				newest = pr
			}
		}
	}
	return newest, nil
}

func (h *Host) listPRs(ctx context.Context, branch, state string) ([]originPR, error) {
	args := append([]string{"pr", "list"}, h.repoArgs()...)
	args = append(args,
		"--head", branch,
		"--state", state,
		"--json", listJSONFields,
		"--limit", "100",
	)
	out, err := h.cmd(ctx, "origin", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("origin pr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("origin pr list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var items []originPR
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("origin pr list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	if items == nil {
		return nil, errors.New("origin pr list: expected JSON array")
	}
	return items, nil
}

func prNumber(pr *scm.PR) int {
	n, _ := strconv.Atoi(pr.Number)
	return n
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	status := "open"
	if h.draft {
		status = "draft"
	}
	args := append([]string{"pr", "create"}, h.repoArgs()...)
	args = append(args,
		"--head", branch,
		"--base", base,
		"--status", status,
		"--title", content.Title,
		"--body-file", "-",
	)
	cmd := h.cmd(ctx, "origin", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("origin pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// The URL the CLI printed identifies the PR this create made; only without
	// one fall back to the newest live PR for the branch.
	if url := extractOriginPRURL(out); url != "" {
		num, _ := parseOriginPRURL(url)
		return &scm.PR{Number: num, URL: url, BaseBranch: base}, nil
	}
	pr, err := h.FindPR(ctx, branch, base)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("origin pr create: could not determine PR URL from output: %s", strings.TrimSpace(string(out)))
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	args := append([]string{"pr", "edit", selector}, h.repoArgs()...)
	if strings.TrimSpace(content.Title) != "" {
		args = append(args, "--title", content.Title)
	}
	args = append(args, "--body-file", "-")
	cmd := h.cmd(ctx, "origin", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("origin pr edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) SetPRBaseBranch(ctx context.Context, pr *scm.PR, baseBranch string) error {
	selector, err := prSelector(pr)
	if err != nil {
		return err
	}
	args := append([]string{"pr", "edit", selector}, h.repoArgs()...)
	args = append(args, "--base", baseBranch)
	if out, err := h.cmd(ctx, "origin", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("origin pr edit --base: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (h *Host) viewPR(ctx context.Context, pr *scm.PR) (originPR, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return originPR{}, err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", viewJSONFields)
	out, err := h.cmd(ctx, "origin", args...).CombinedOutput()
	if err != nil {
		return originPR{}, fmt.Errorf("origin pr view: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return originPR{}, fmt.Errorf("origin pr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var view originPR
	if err := json.Unmarshal(trimmed, &view); err != nil {
		return originPR{}, fmt.Errorf("origin pr view: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return view, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	view, err := h.viewPR(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizeOriginPRState(view.Status), nil
}

func (h *Host) GetPRBaseBranch(ctx context.Context, pr *scm.PR) (string, error) {
	view, err := h.viewPR(ctx, pr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(view.BaseRef), nil
}

// GetPRContent reads the raw markdown description from `origin pr view
// --json`. A missing or null description is unproven, not an empty body.
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	view, err := h.viewPR(ctx, pr)
	if err != nil {
		return scm.PRContent{}, err
	}
	if (pr.Number != "" && view.number() != strings.TrimSpace(pr.Number)) || strings.TrimSpace(view.Title) == "" || view.Description == nil {
		return scm.PRContent{}, errors.New("origin pr view: incomplete or mismatched raw content")
	}
	return scm.PRContent{Title: view.Title, Body: *view.Description}, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	view, err := h.viewPR(ctx, pr)
	if err != nil {
		return "", err
	}
	m := view.Mergeability
	if m.HasMergeConflicts != nil && *m.HasMergeConflicts {
		return scm.MergeableConflict, nil
	}
	if m.Mergeable != nil && *m.Mergeable {
		return scm.MergeableOK, nil
	}
	// hasMergeConflicts absent or false and not mergeable: only an explicit
	// conflict signal is CONFLICTING. Other blockers (draft, rule failure,
	// nothing to merge) are not conflicts and must not hold the CI monitor
	// pending, so they read as MERGEABLE.
	if len(m.ConflictedPaths) > 0 || strings.Contains(strings.ToLower(m.Mergeability.Verdict), "conflict") {
		return scm.MergeableConflict, nil
	}
	// No mergeability result at all is unproven, never MERGEABLE.
	if m.Mergeable == nil && m.HasMergeConflicts == nil && m.Mergeability.Verdict == "" {
		return scm.MergeableUnknown, nil
	}
	return scm.MergeableOK, nil
}

type originCheck struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	DetailsURL  string `json:"detailsUrl"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	args := append([]string{"pr", "checks", selector}, h.repoArgs()...)
	args = append(args, "--json", checkJSONFields)
	out, err := h.cmd(ctx, "origin", args...).CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		if strings.Contains(msg, "no checks") || strings.Contains(msg, "no ci") {
			return nil, nil
		}
		return nil, fmt.Errorf("origin pr checks: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		if strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("origin pr checks: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var raw []originCheck
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, fmt.Errorf("origin pr checks: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, r := range raw {
		checks = append(checks, scm.Check{
			Name:        r.Name,
			ProviderID:  originCheckID(r.ID),
			Bucket:      originCheckBucket(r.Status, r.Conclusion),
			State:       strings.ToUpper(strings.TrimSpace(r.Conclusion)),
			StartedAt:   parseOriginTime(r.StartedAt),
			CompletedAt: parseOriginTime(r.CompletedAt),
			Link:        strings.TrimSpace(r.DetailsURL),
		})
	}
	return checks, nil
}

func originCheckID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return "origin-check:" + id
}

func (h *Host) FetchFailedCheckLogs(context.Context, *scm.PR, string, string, []string) (string, error) {
	return "", scm.ErrUnsupported
}

func parseOriginTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

func originCheckBucket(status, conclusion string) scm.CheckBucket {
	if strings.ToLower(strings.TrimSpace(status)) != "completed" {
		return scm.CheckBucketPending
	}
	switch strings.ToUpper(strings.TrimSpace(conclusion)) {
	case "SUCCESS":
		return scm.CheckBucketPass
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return scm.CheckBucketFail
	case "CANCELLED", "CANCELED":
		return scm.CheckBucketCancel
	case "SKIPPED", "NEUTRAL", "STALE":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}

func normalizeOriginPRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "open", "draft":
		return scm.PRStateOpen
	case "merged":
		return scm.PRStateMerged
	case "closed":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(raw))
	}
}

func parseOriginPRURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("expected absolute Origin pull request URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", errors.New("expected HTTP Origin pull request URL")
	}
	host := strings.ToLower(parsed.Hostname())
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	switch {
	case host == "cursor.com" && len(segments) == 5 && segments[0] == "codebase" && segments[3] == "pull":
		// https://cursor.com/codebase/owner/repo/pull/N
	case (host == "origin.cursor.com" || strings.HasSuffix(host, ".origin.cursor.com")) && len(segments) == 4 && segments[2] == "pull":
		// https://origin.cursor.com/owner/repo/pull/N (not observed live; accepted)
	default:
		return "", errors.New("expected Origin /codebase/owner/repo/pull/number URL")
	}
	num := segments[len(segments)-1]
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", errors.New("expected positive Origin pull request number")
	}
	if parsed.RawQuery != "" || strings.Contains(trimmed, "#") {
		return "", errors.New("expected Origin pull request URL without query or fragment")
	}
	return num, nil
}

func bytesTrimToJSON(out []byte) []byte {
	idx := -1
	for i, b := range out {
		if b == '{' || b == '[' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	return out[idx:]
}

func extractOriginPRURL(out []byte) string {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if (strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) && !strings.ContainsAny(line, " \t") {
			if _, err := parseOriginPRURL(line); err == nil {
				return line
			}
		}
	}
	return ""
}

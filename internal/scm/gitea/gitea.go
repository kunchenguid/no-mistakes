// Package gitea implements scm.Host backed by the tea CLI (Gitea's official
// CLI).
//
// Unlike gh/glab, tea has no notion of "the current instance": it infers
// context from the working directory's git remote, which the daemon's
// detached bare-gate repo does not have. Every invocation here therefore
// selects the account and --repo explicitly (see the New doc comment).
//
// Two incompatible tea generations exist, and both are supported. tea 0.x
// (urfave/cli, up to v0.16.0) selects an account with --login <name>, asks for
// JSON with --output json (plus --fields on lists), and names its commands
// `pulls`, `pulls <n>`, and `actions runs`. tea 1.0 (the Cobra rewrite,
// gitea/tea#1126) removed all of those with no compatibility aliases: the
// account is --host <instance url>, JSON is --json=<fields> (the `=` form is
// required because the flag has an optional value, and unknown field names
// are rejected), and the commands are `pr list|view|create|edit` and `run
// list|logs`. The generation is detected once per Host by a feature probe
// (Host.generation), never by `tea --version`: builds from main report a
// `git describe` string such as 0.16.0+30-g..., and source builds report
// "development", so a version check would misread exactly the binaries most
// likely to be 1.0 today. Further 1.0 traps handled below: `tea api -F/--field`
// is TYPED (a branch named `123` or `true` would be sent as a number or a
// bool), so retargeting uses -f/--raw-field; `tea run logs` prints its "Job N"
// banner to stderr, so stdout is the raw log and must not be cut at a `---`
// line the way tea 0.x's banner is; and an empty --host silently falls back to
// TEA_HOST, the default host, or the sole configured instance, so Available
// refuses a missing instance before any call is made.
//
// `tea actions runs view --jobs --output json` was empirically verified
// (against a real tea 0.15.1 CLI and a real Gitea 1.27.2 + Actions instance)
// to NOT emit clean structured per-job JSON: --output json only formats the
// run-level header block as JSON-ish text mixed with a genuine JSON jobs
// array, and that jobs array carries no `conclusion` field (only `status`),
// so pass/fail cannot be read from it. The REST job endpoint
// (GET /repos/{owner}/{repo}/actions/runs/{run}/jobs) does carry a `status` +
// `conclusion` pair (mirroring GitHub Actions' schema) and is reachable
// through `tea api`, which reuses tea's own stored login/token - no separate
// HTTP client or token configuration is needed. GetChecks,
// FetchFailedCheckLogs, and SetPRBaseBranch therefore go through `tea api`
// (the last because tea 0.x's `tea pulls edit` has no `--base` flag; tea 1.0's
// `pr edit` has one, but one retarget path for both generations is simpler to
// keep correct) while every other operation (PR lifecycle, auth, run
// discovery) uses tea's own porcelain subcommands.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to Gitea through the tea CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	host         string // repo's Gitea hostname; used only for error messages
	login        string // tea 0.x login name for this host; scopes every tea 0.x invocation
	instance     string // tea 1.0 instance URL for this host; scopes every tea 1.0 invocation
	repoSlug     string // "owner/repo" slug tea's --repo flag expects

	mu  sync.Mutex
	gen teaGeneration // cached once a probe succeeds
}

// teaGeneration is the tea CLI generation Host.generation detected.
type teaGeneration int

const (
	teaUnprobed teaGeneration = iota
	tea0                      // tea 0.x: --login, --output json, `pulls`, `actions runs`
	tea1                      // tea 1.0: --host, --json=<fields>, `pr`, `run`
)

// New builds a Host. cliAvailable reports whether the tea binary is
// resolvable on the caller's PATH. host is the repo's Gitea hostname, used
// only to produce an actionable error message. account is the tea account
// configured for that host in each generation's own config (see
// scm.ResolveTeaAccount): tea requires an explicit account on every command
// run outside a directory whose git remote it recognizes, which the daemon's
// detached bare-gate repo never is. repoSlug is the "owner/repo" path tea's
// --repo flag expects (no host prefix; the account already disambiguates the
// instance). All are optional; an account missing for the installed tea
// generation makes Available fail closed with a setup hint instead of
// silently falling back to tea's own default-account guessing.
func New(cmd CmdFactory, cliAvailable func() bool, host string, account scm.TeaAccount, repoSlug string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		host:         strings.TrimSpace(host),
		login:        strings.TrimSpace(account.Login),
		instance:     strings.TrimSpace(account.Instance),
		repoSlug:     strings.TrimSpace(repoSlug),
	}
}

// generation detects the installed tea generation with two feature probes,
// tea 1.0's first: `tea pr list --json=number --help` exits 0 only on tea 1.0
// (tea 0.x has no `pr list --json` and exits 1), and `tea pulls list --output
// json --help` exits 0 only on tea 0.x (tea 1.0 has no `pulls` and exits 2).
// --help makes both side-effect free: neither generation reads its config,
// touches the network, or prompts for a help request, so the probe gives the
// same answer with or without a configured account. A binary that passes
// neither probe is refused with an error rather than guessed at, and only a
// successful probe is cached, so a cancelled or failed probe is retried on
// the next call instead of pinning a wrong answer.
func (h *Host) generation(ctx context.Context) (teaGeneration, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != teaUnprobed {
		return h.gen, nil
	}
	if h.cmd(ctx, "tea", "pr", "list", "--json=number", "--help").Run() == nil {
		h.gen = tea1
		return h.gen, nil
	}
	if err := ctx.Err(); err != nil {
		return teaUnprobed, err
	}
	if h.cmd(ctx, "tea", "pulls", "list", "--output", "json", "--help").Run() == nil {
		h.gen = tea0
		return h.gen, nil
	}
	if err := ctx.Err(); err != nil {
		return teaUnprobed, err
	}
	return teaUnprobed, errors.New("unrecognized tea CLI: it accepts neither tea 1.0's `tea pr list --json` nor tea 0.x's `tea pulls list --output json`; install a released tea or a tea 1.0 build")
}

// account returns the flag pair that selects this host's account for gen.
// Available has already refused an empty one, so a caller never passes tea an
// empty --login or --host (tea 1.0 would silently pick another instance).
func (h *Host) account(gen teaGeneration) []string {
	if gen == tea1 {
		return []string{"--host", h.instance}
	}
	return []string{"--login", h.login}
}

// begin resolves the generation and its account flags for one operation.
func (h *Host) begin(ctx context.Context) (teaGeneration, []string, error) {
	gen, err := h.generation(ctx)
	if err != nil {
		return teaUnprobed, nil, err
	}
	if gen == tea1 && h.instance == "" || gen == tea0 && h.login == "" {
		return gen, nil, h.missingAccountError(gen)
	}
	return gen, h.account(gen), nil
}

func (h *Host) missingAccountError(gen teaGeneration) error {
	host := h.host
	if host == "" {
		host = "<gitea-host>"
	}
	if gen == tea1 {
		return fmt.Errorf("no tea 1.0 account configured for %s (tea 1.0 does not read tea 0.x logins); run `tea auth login --host https://%s --with-token < token-file`", host, host)
	}
	return fmt.Errorf("no tea login configured for %s; run `tea logins add --url https://%s --token <token> --name <name>`", host, host)
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitea }

func (h *Host) Capabilities() scm.Capabilities {
	// MergeableState is declined: Gitea's PR `mergeable` field has a
	// documented upstream reliability bug (go-gitea/gitea#25849) that can
	// stick `false` after a conflict is actually resolved. Trusting it is
	// worse than declining the capability, matching Bitbucket's posture.
	return scm.Capabilities{MergeableState: false, FailedCheckLogs: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("tea CLI is not installed")
	}
	gen, account, err := h.begin(ctx)
	if err != nil {
		return err
	}
	// `tea whoami` has no --login flag (and is gone in tea 1.0), so it cannot
	// be scoped to a specific host the way `glab auth status --hostname` can.
	// `tea api` accepts the account flag on every call in both generations, so
	// an authenticated GET /user through it is the host-scoped equivalent: it
	// fails when this account's token is missing or invalid without being
	// influenced by any other configured tea account.
	args := append([]string{"api"}, account...)
	out, err := h.cmd(ctx, "tea", append(args, "/user")...).CombinedOutput()
	if err != nil {
		if gen == tea1 {
			// tea 1.0 refuses a leftover tea 0.x config.yml outright; its
			// message says how to move it aside, so pass it through.
			return fmt.Errorf("tea CLI is not authenticated for %s: %s", h.instance, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("tea CLI is not authenticated for login %q", h.login)
	}
	return nil
}

// giteaPRListItem is one element of tea 0.x's `tea pulls list --output json`,
// which renders every field (index included) as a JSON string. tea 0.x's
// single-PR view renders the same fields with native JSON types
// (giteaPRView), so the two shapes are deliberately separate structs; do not
// unify them. tea 1.0 shares one shape (teaPR) between list and view, which
// is converted into these two.
type giteaPRListItem struct {
	Index string `json:"index"`
	State string `json:"state"`
	URL   string `json:"url"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// teaPR is tea 1.0's pull request JSON, the same object for `pr list` and
// `pr view`, with native types (number is an integer).
type teaPR struct {
	Number  int64  `json:"number"`
	State   string `json:"state"`
	URL     string `json:"url"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	HeadSHA string `json:"head_sha"`
	Merged  bool   `json:"merged"`
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	gen, account, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	// The list is filtered client-side (tea 0.x has no --head/--base filter).
	// --state is left at its default (open), matching every other provider's
	// FindPR: only an open PR for the branch counts.
	label := "tea pulls list"
	args := append([]string{"pulls", "list", "--repo", h.repoSlug}, account...)
	args = append(args, "--fields", "index,title,state,url,head,base", "--output", "json")
	if gen == tea1 {
		label = "tea pr list"
		args = append([]string{"pr", "list", "--repo", h.repoSlug}, account...)
		args = append(args, "--json=number,state,url,head,base")
	}
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", label, strings.TrimSpace(string(out)), err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		// Nonempty output with no JSON delimiter is not a legitimate "no
		// open PRs" response (an empty list still prints "[]", which has a
		// delimiter) - it must surface as an error rather than be read as
		// absence, which would otherwise cause the PR step to attempt a
		// duplicate create or report a misleading creation failure.
		return nil, fmt.Errorf("%s: invalid JSON output: %s", label, strings.TrimSpace(string(out)))
	}
	var items []giteaPRListItem
	if gen == tea1 {
		var prs []teaPR
		err = json.Unmarshal(trimmed, &prs)
		for _, pr := range prs {
			items = append(items, giteaPRListItem{Index: strconv.FormatInt(pr.Number, 10), State: pr.State, URL: pr.URL, Head: pr.Head, Base: pr.Base})
		}
	} else {
		err = json.Unmarshal(trimmed, &items)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: invalid JSON output: %s", label, strings.TrimSpace(string(out)))
	}
	base = strings.TrimSpace(base)
	for _, item := range items {
		if item.Head != branch {
			continue
		}
		if base != "" && item.Base != base {
			continue
		}
		return &scm.PR{Number: item.Index, URL: item.URL, BaseBranch: strings.TrimSpace(item.Base)}, nil
	}
	return nil, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	gen, account, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	label, verb, bodyFlag := "tea pulls create", []string{"pulls", "create"}, "--description"
	if gen == tea1 {
		label, verb, bodyFlag = "tea pr create", []string{"pr", "create"}, "--body"
	}
	args := append(append(verb, "--repo", h.repoSlug), account...)
	args = append(args,
		"--head", branch,
		"--base", base,
		"--title", content.Title,
		bodyFlag, content.Body,
	)
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", label, strings.TrimSpace(string(out)), err)
	}
	// tea 0.x's pulls create has no --output json flag, so the robust path is
	// to re-list the PR by head branch rather than scrape the human-readable
	// create output, which echoes the PR body and can itself contain
	// http(s) URLs that would confuse a naive "first URL line" scan. The same
	// re-list serves tea 1.0, so both generations read one structured shape.
	if pr, ferr := h.FindPR(ctx, branch, base); ferr == nil && pr != nil {
		return pr, nil
	}
	url := extractGiteaPRURL(out)
	if url == "" {
		return nil, fmt.Errorf("%s: could not determine PR URL from output: %s", label, strings.TrimSpace(string(out)))
	}
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := ""
	if pr != nil {
		id = pr.Number
		if id == "" {
			if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
				id = num
			}
		}
		if id == "" {
			id = pr.URL
		}
	}
	gen, account, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	label, verb, bodyFlag := "tea pulls edit", []string{"pulls", "edit", id}, "--description"
	if gen == tea1 {
		label, verb, bodyFlag = "tea pr edit", []string{"pr", "edit", id}, "--body"
	}
	args := append(append(verb, "--repo", h.repoSlug), account...)
	if content.Title != "" {
		args = append(args, "--title", content.Title)
	}
	args = append(args, bodyFlag, content.Body)
	if out, err := h.cmd(ctx, "tea", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", label, strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

// SetPRBaseBranch retargets an existing PR. `tea pulls edit` (tea 0.15.1) has
// no `--base` flag even though `tea pulls create` does; inventing one would
// fail closed at the CLI. The Gitea REST edit-pull endpoint accepts `base`,
// and `tea api --method PATCH` reuses the stored account the same way job
// lookups do. `--method PATCH` is required: a body without an explicit
// method defaults tea api to POST. tea 0.x's `--field` sends a string; tea
// 1.0's `--field` is typed, so tea 1.0 gets `--raw-field` to keep a branch
// named `123` or `true` a string.
func (h *Host) SetPRBaseBranch(ctx context.Context, pr *scm.PR, baseBranch string) error {
	id, err := giteaPRNumber(pr)
	if err != nil {
		return err
	}
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok {
		return fmt.Errorf("gitea: invalid repo slug %q", h.repoSlug)
	}
	gen, account, err := h.begin(ctx)
	if err != nil {
		return err
	}
	fieldFlag := "--field"
	if gen == tea1 {
		fieldFlag = "--raw-field"
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls/%s", owner, repo, id)
	args := append([]string{"api"}, account...)
	args = append(args, "--method", "PATCH", fieldFlag, "base="+baseBranch, endpoint)
	if out, err := h.cmd(ctx, "tea", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tea api pull edit --base: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func giteaPRNumber(pr *scm.PR) (string, error) {
	if pr != nil {
		if id := strings.TrimSpace(pr.Number); id != "" {
			return id, nil
		}
		if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
			return num, nil
		}
	}
	return "", fmt.Errorf("pull request identity is required to retarget")
}

type giteaPRView struct {
	Index     int    `json:"index"`
	State     string `json:"state"`
	URL       string `json:"url"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	HeadSHA   string `json:"headSha"`
	Mergeable bool   `json:"mergeable"`
	HasMerged bool   `json:"hasMerged"`
}

func (h *Host) viewPR(ctx context.Context, number string) (giteaPRView, error) {
	gen, account, err := h.begin(ctx)
	if err != nil {
		return giteaPRView{}, err
	}
	label := "tea pulls view"
	args := append([]string{"pulls", strings.TrimSpace(number), "--repo", h.repoSlug}, account...)
	args = append(args, "--output", "json")
	if gen == tea1 {
		label = "tea pr view"
		args = append([]string{"pr", "view", strings.TrimSpace(number), "--repo", h.repoSlug}, account...)
		args = append(args, "--json=number,state,url,head,base,head_sha,merged")
	}
	out, err := h.cmd(ctx, "tea", args...).CombinedOutput()
	if err != nil {
		return giteaPRView{}, fmt.Errorf("%s: %s: %w", label, strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return giteaPRView{}, fmt.Errorf("%s: invalid JSON output: %s", label, strings.TrimSpace(string(out)))
	}
	var view giteaPRView
	if gen == tea1 {
		var pr teaPR
		err = json.Unmarshal(trimmed, &pr)
		view = giteaPRView{Index: int(pr.Number), State: pr.State, URL: pr.URL, Head: pr.Head, Base: pr.Base, HeadSHA: pr.HeadSHA, HasMerged: pr.Merged}
	} else {
		err = json.Unmarshal(trimmed, &view)
	}
	if err != nil {
		return giteaPRView{}, fmt.Errorf("%s: invalid JSON output: %s", label, strings.TrimSpace(string(out)))
	}
	return view, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	view, err := h.viewPR(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	// Gitea reports state="closed" AND hasMerged=true for a merged PR; check
	// hasMerged first so a merged PR is never read as plain CLOSED.
	if view.HasMerged {
		return scm.PRStateMerged, nil
	}
	return normalizeGiteaPRState(view.State), nil
}

// GetMergeableState is unsupported: see the Capabilities doc comment.
func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	return "", scm.ErrUnsupported
}

type giteaRunPullRequest struct {
	Number int `json:"number"`
}

type giteaRunSummary struct {
	ID           giteaRunID            `json:"id"`
	Status       string                `json:"status"`
	HeadBranch   string                `json:"head_branch"`
	Event        string                `json:"event"`
	PullRequests []giteaRunPullRequest `json:"pull_requests"`
}

// giteaRunID unmarshals a workflow run's id from either a bare JSON number
// (the raw REST `/actions/runs` endpoint's own type, confirmed against a
// live Gitea 1.27.3 instance) or a JSON string, storing it as a decimal
// string: every call site here uses a run id as a tea/REST path segment,
// never for arithmetic.
type giteaRunID string

func (id *giteaRunID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) >= 2 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*id = giteaRunID(s)
		return nil
	}
	*id = giteaRunID(data)
	return nil
}

// listRuns lists the repository's most recent workflow runs for the caller
// to match against a PR (see runJobsMatchingHeadSHA). It calls the raw
// `/actions/runs` REST endpoint directly through `tea api` - the same
// generation-agnostic passthrough runJobs already uses for job details -
// instead of tea's own dedicated `actions runs list`/`run list` commands,
// because those commands' own JSON output drops two fields this code needs:
// a pull_request-triggered run's real branch name (Gitea's REST response
// names it in `head_branch` as the literal "#<PR number>", which tea's
// dedicated commands either blank out or never request at all) and the
// run's own `pull_requests` array, which names the exact PR number Gitea
// itself associated with that run. Confirmed directly against a live Gitea
// 1.27.3 instance. Matching on pull_requests[].number (see
// runJobsMatchingHeadSHA) makes a cross-PR or cross-branch collision
// structurally impossible rather than merely unlikely, which a branch-name
// or bare head-SHA match alone cannot guarantee. `limit=50` raises the
// single page fetched above the default page size to reduce, though on a
// sufficiently busy repository not eliminate, the chance that the matching
// run falls off the first page.
func (h *Host) listRuns(ctx context.Context) ([]giteaRunSummary, error) {
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok {
		return nil, fmt.Errorf("gitea: invalid repo slug %q", h.repoSlug)
	}
	_, account, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/actions/runs?limit=50", owner, repo)
	args := append([]string{"api"}, account...)
	out, err := h.cmd(ctx, "tea", append(args, endpoint)...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tea api actions runs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var resp struct {
		WorkflowRuns []giteaRunSummary `json:"workflow_runs"`
	}
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, fmt.Errorf("tea api actions runs: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return resp.WorkflowRuns, nil
}

type giteaJob struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	HeadSHA     string `json:"head_sha"`
	HTMLURL     string `json:"html_url"`
	CompletedAt string `json:"completed_at"`
}

// completedAt parses the job's completed_at timestamp (RFC3339, GitHub-Actions-
// schema style), returning the zero time when absent or unparseable.
func (j giteaJob) completedAt() time.Time {
	if strings.TrimSpace(j.CompletedAt) == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, j.CompletedAt); err == nil {
		return parsed
	}
	return time.Time{}
}

func (h *Host) runJobs(ctx context.Context, runID string) ([]giteaJob, error) {
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok {
		return nil, fmt.Errorf("gitea: invalid repo slug %q", h.repoSlug)
	}
	_, account, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/actions/runs/%s/jobs", owner, repo, runID)
	args := append([]string{"api"}, account...)
	out, err := h.cmd(ctx, "tea", append(args, endpoint)...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tea api actions runs jobs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	trimmed := bytesTrimToJSON(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var resp struct {
		Jobs []giteaJob `json:"jobs"`
	}
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, fmt.Errorf("tea api actions runs jobs: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return resp.Jobs, nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	view, err := h.viewPR(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(view.Head) == "" {
		return nil, nil
	}
	runs, err := h.listRuns(ctx)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	_, jobs, err := h.runJobsMatchingHeadSHA(ctx, runs, view.HeadSHA, view.Head, view.Index)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	return jobsToChecks(jobs), nil
}

// runsByIDDesc orders runs by highest numeric ID first rather than trusting
// `tea actions runs list`'s array order, which is not documented as
// newest-first. A run whose ID does not parse as an integer sorts last so it
// can never mask a numerically comparable run.
func runsByIDDesc(runs []giteaRunSummary) []giteaRunSummary {
	ordered := make([]giteaRunSummary, len(runs))
	copy(ordered, runs)
	sort.SliceStable(ordered, func(i, j int) bool {
		idI, okI := parseGiteaRunID(string(ordered[i].ID))
		idJ, okJ := parseGiteaRunID(string(ordered[j].ID))
		if okI && okJ {
			return idI > idJ
		}
		return okI && !okJ
	})
	return ordered
}

// runJobsMatchingHeadSHA finds the run (and its jobs) whose jobs belong to
// headSHA, searching candidate runs from highest ID to lowest. A PR can have
// more than one matching run - e.g. a manual re-run of an older commit can
// land a higher run ID than a newer commit's own run - so trusting the
// highest-ID run alone (or list order) could silently attribute a stale
// commit's checks/logs to the PR's current head, or miss a matching
// lower-ID run entirely.
//
// prNumber and sourceBranch scope the candidate list to runs that can
// actually belong to this PR before any SHA matching starts (see
// filterRunsForPR): a pull_request-triggered run is kept only when its own
// pull_requests array names this exact PR number, and a push-triggered run
// (e.g. from a plain push to the PR's own branch before or after the PR
// opened) is kept only when its head branch equals sourceBranch. Because
// that scoping is exact rather than best-effort, it is safe for the
// empty-headSHA case to fall back to the newest (highest-ID) run in the
// already-scoped list: no run outside this PR can ever reach that fallback.
func (h *Host) runJobsMatchingHeadSHA(ctx context.Context, runs []giteaRunSummary, headSHA, sourceBranch string, prNumber int) (giteaRunSummary, []giteaJob, error) {
	ordered := runsByIDDesc(filterRunsForPR(runs, sourceBranch, prNumber))
	if len(ordered) == 0 {
		return giteaRunSummary{}, nil, nil
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		run := ordered[0]
		jobs, err := h.runJobs(ctx, string(run.ID))
		return run, jobs, err
	}
	for _, run := range ordered {
		jobs, err := h.runJobs(ctx, string(run.ID))
		if err != nil {
			return giteaRunSummary{}, nil, err
		}
		if anyJobMatchesHeadSHA(jobs, headSHA) {
			return run, jobs, nil
		}
	}
	// No run yet matches the PR's current head commit: CI has not caught up
	// with the latest push, or only stale/unrelated runs exist so far.
	return giteaRunSummary{}, nil, nil
}

// filterRunsForPR keeps only runs that can plausibly belong to this PR. A
// pull_request-triggered run is kept only when its own pull_requests array
// names prNumber - the one signal Gitea's API provides for that case, since
// such a run's head branch is either absent or the literal "#<PR number>"
// rather than a real branch name (see listRuns). Any other run (in
// practice, a push-triggered run) is kept only when its HeadBranch equals
// sourceBranch. A run whose event is "pull_request" but whose pull_requests
// array does not name prNumber is dropped outright rather than falling
// through to the branch check, since such a run's HeadBranch is never a
// usable branch name either.
func filterRunsForPR(runs []giteaRunSummary, sourceBranch string, prNumber int) []giteaRunSummary {
	sourceBranch = strings.TrimSpace(sourceBranch)
	filtered := make([]giteaRunSummary, 0, len(runs))
	for _, run := range runs {
		if run.Event == "pull_request" {
			if runNamesPR(run, prNumber) {
				filtered = append(filtered, run)
			}
			continue
		}
		if run.HeadBranch == sourceBranch {
			filtered = append(filtered, run)
		}
	}
	return filtered
}

// runNamesPR reports whether run's own pull_requests array names prNumber.
func runNamesPR(run giteaRunSummary, prNumber int) bool {
	if prNumber <= 0 {
		return false
	}
	for _, pr := range run.PullRequests {
		if pr.Number == prNumber {
			return true
		}
	}
	return false
}

func parseGiteaRunID(id string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func anyJobMatchesHeadSHA(jobs []giteaJob, sha string) bool {
	for _, j := range jobs {
		if j.HeadSHA == sha {
			return true
		}
	}
	return false
}

func jobsToChecks(jobs []giteaJob) []scm.Check {
	checks := make([]scm.Check, 0, len(jobs))
	for _, job := range jobs {
		providerID := ""
		if job.ID != 0 {
			providerID = fmt.Sprintf("gitea-job:%d", job.ID)
		}
		checks = append(checks, scm.Check{
			Name:        job.Name,
			ProviderID:  providerID,
			Bucket:      giteaStatusBucket(job.Status, job.Conclusion),
			CompletedAt: job.completedAt(),
			Link:        job.HTMLURL,
		})
	}
	return checks
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	targets := make([]scm.CheckTarget, 0, len(failingNames))
	for _, name := range failingNames {
		targets = append(targets, scm.CheckTarget{Name: name})
	}
	logs, err := h.FetchFailedCheckTargetLogs(ctx, pr, branch, headSHA, targets)
	if err != nil {
		return "", err
	}
	return scm.CombineFailedCheckLogs(logs)
}

func (h *Host) FetchFailedCheckTargetLogs(ctx context.Context, pr *scm.PR, _ string, headSHA string, targets []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	view, err := h.viewPR(ctx, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("resolve Gitea pull request for selected logs: %w", err)
	}
	if strings.TrimSpace(view.Head) == "" {
		return nil, errors.New("resolve Gitea pull request for selected logs: head branch is empty")
	}
	runs, err := h.listRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Gitea runs for selected logs: %w", err)
	}
	if len(runs) == 0 {
		return nil, errors.New("no Gitea runs found for selected logs")
	}
	matchSHA := strings.TrimSpace(headSHA)
	if matchSHA == "" {
		matchSHA = view.HeadSHA
	}
	run, jobs, err := h.runJobsMatchingHeadSHA(ctx, runs, matchSHA, view.Head, view.Index)
	if err != nil {
		return nil, fmt.Errorf("find Gitea run for selected logs: %w", err)
	}
	if run.ID == "" {
		return nil, errors.New("no Gitea run matched the selected log target head")
	}
	gen, account, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]scm.FailedCheckLog, 0, len(targets))
	for _, target := range targets {
		result := scm.FailedCheckLog{Target: target}
		jobIDs := findFailedGiteaJobTargetIDs(jobs, []scm.CheckTarget{target})
		if len(jobIDs) == 0 {
			result.Err = fmt.Errorf("selected Gitea check %q was not found", target.Identity())
			results = append(results, result)
			continue
		}
		var outputs []string
		var errs []error
		for _, jobID := range jobIDs {
			verb := []string{"actions", "runs", "logs", string(run.ID)}
			if gen == tea1 {
				verb = []string{"run", "logs", string(run.ID)}
			}
			args := append(verb, "--job", strconv.Itoa(jobID), "--repo", h.repoSlug)
			out, err := h.cmd(ctx, "tea", append(args, account...)...).Output()
			if err != nil {
				errs = append(errs, fmt.Errorf("fetch Gitea job %d log: %w", jobID, err))
				continue
			}
			// tea 1.0 prints its "Job N" banner to stderr, which Output
			// discards, so its stdout is the log itself and must not be cut
			// at a `---` line inside it.
			log := strings.TrimSpace(string(out))
			if gen == tea0 {
				log = stripGiteaLogsHeader(string(out))
			}
			if log != "" {
				outputs = append(outputs, log)
			}
		}
		result.Output = strings.Join(outputs, "\n\n")
		result.Err = errors.Join(errs...)
		results = append(results, result)
	}
	return results, nil
}

func findFailedGiteaJobTargetIDs(jobs []giteaJob, checkTargets []scm.CheckTarget) []int {
	names := map[string]struct{}{}
	ids := map[string]struct{}{}
	for _, target := range checkTargets {
		if id := strings.TrimSpace(target.ProviderID); id != "" {
			ids[id] = struct{}{}
		} else if name := strings.TrimSpace(target.Name); name != "" {
			names[name] = struct{}{}
		}
	}
	var matched []int
	for _, job := range jobs {
		if giteaStatusBucket(job.Status, job.Conclusion) != scm.CheckBucketFail {
			continue
		}
		_, nameMatch := names[job.Name]
		_, idMatch := ids[fmt.Sprintf("gitea-job:%d", job.ID)]
		if nameMatch || idMatch || len(names)+len(ids) == 0 {
			matched = append(matched, job.ID)
		}
	}
	return matched
}

// stripGiteaLogsHeader removes the "Logs for job N:\n---\n" banner that tea
// 0.x's `tea actions runs logs` prints before the actual log body.
func stripGiteaLogsHeader(s string) string {
	if idx := strings.Index(s, "---\n"); idx >= 0 {
		return strings.TrimSpace(s[idx+len("---\n"):])
	}
	return strings.TrimSpace(s)
}

// splitOwnerRepo splits a "owner/repo" slug (Gitea has no nested subgroups,
// unlike GitLab) into its two path segments.
func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	slug = strings.Trim(strings.TrimSpace(slug), "/")
	if slug == "" {
		return "", "", false
	}
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// bytesTrimToJSON skips any banner text before the first '{' or '[', mirroring
// the same defensive scan the gitlab CLI-shelled Host uses: a version-check
// notice or similar stray line before the JSON payload must not break parsing.
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

// extractGiteaPRURL scans out (tea pulls create's human-readable stdout) from
// the end for the last line that is a bare http(s) URL - the trailing summary
// line `tea` always prints after creating a PR. It is a fallback for
// CreatePR only; the primary path re-lists the PR for a structured result.
func extractGiteaPRURL(out []byte) string {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if (strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) && !strings.ContainsAny(line, " \t") {
			return line
		}
	}
	return ""
}

func giteaStatusBucket(status, conclusion string) scm.CheckBucket {
	if strings.ToLower(strings.TrimSpace(status)) != "completed" {
		return scm.CheckBucketPending
	}
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "success":
		return scm.CheckBucketPass
	case "failure":
		return scm.CheckBucketFail
	case "cancelled", "canceled":
		return scm.CheckBucketCancel
	case "skipped":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}

func normalizeGiteaPRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "open":
		return scm.PRStateOpen
	case "closed":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(raw))
	}
}

// Package plugin implements scm.Host by delegating every provider operation to
// an operator-configured executable that is an AXI-shaped CLI: subcommands
// such as "pr find" or "pr checks" with --json, one process per operation,
// one JSON document on stdout. It is driven the same way no-mistakes drives
// forgejo-axi, and lets a machine add PR and CI support for a forge
// no-mistakes does not ship (for example a private or company-internal host)
// without changing this repository.
//
// The adapter trusts nothing the plugin returns: every identity (PR number,
// URL, head and base branch) is re-checked against what was asked for, and
// any missing, malformed, or contradictory answer is an ErrProtocol error
// rather than a guess, so a buggy plugin can never make the PR step open a
// duplicate PR or the CI step certify the wrong commit.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const (
	// DefaultTimeout bounds one plugin invocation when the operator did not
	// configure provider_plugins.<name>.timeout.
	DefaultTimeout = 2 * time.Minute

	maxResponseBytes     = 1 << 20
	maxLogsResponseBytes = 4 << 20
	maxLogsBytes         = 1 << 20
	maxStderrBytes       = 64 << 10
	maxErrorMessageBytes = 4 << 10
)

// CmdFactory creates one non-shell invocation of the plugin executable.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Options configures a plugin host.
type Options struct {
	// Name is the provider_plugins key. It is reported as provider
	// "plugin:<name>" and sent to the plugin as --plugin on every call.
	Name string
	// Executable and Args are the configured command. Args are passed
	// verbatim before the subcommand words. PR bodies never travel in argv:
	// they go through --body-file, so a body can neither hit an argv limit
	// nor show up in a process listing.
	Executable string
	Args       []string
	Repository Repository
	// Timeout bounds each invocation. Zero means DefaultTimeout.
	Timeout           time.Duration
	DraftPullRequests bool
	CommandFactory    CmdFactory
	// ExecutableAvailable reports whether Executable resolves in the run's
	// environment. Nil assumes it does and lets the spawn report otherwise.
	ExecutableAvailable func(string) bool
	// BodyDir is where the private --body-file is created. Empty means the
	// system temporary directory. Never the worktree: the file must not
	// become part of the pushed branch's tree.
	BodyDir string
}

// Host is a scm.Host backed by a provider plugin executable.
type Host struct {
	opts Options
	// run replaces runProcess in this package's tests, so output validation
	// can be exercised without a process per case. args excludes the
	// executable and starts with the configured Args.
	run func(ctx context.Context, args []string, maxStdout int) (invocation, error)

	mu           sync.Mutex
	capabilities scm.Capabilities
	canRetarget  bool
	maxBodyChars int
}

var (
	_ scm.Host               = (*Host)(nil)
	_ scm.PRContentReader    = (*Host)(nil)
	_ scm.PRBaseBranchReader = (*Host)(nil)
	_ scm.PRBaseRetargeter   = (*Host)(nil)
	_ scm.MergedProofHost    = (*Host)(nil)
	_ scm.PRBodyLimiter      = (*Host)(nil)
)

// New constructs a plugin host.
func New(opts Options) *Host {
	opts.Name = strings.TrimSpace(opts.Name)
	opts.Executable = strings.TrimSpace(opts.Executable)
	opts.Args = append([]string(nil), opts.Args...)
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	return &Host{opts: opts}
}

func (h *Host) Provider() scm.Provider { return scm.PluginProvider(h.opts.Name) }

// Capabilities reports what the plugin declared in its last successful
// handshake. Before Available succeeds every optional capability is off.
func (h *Host) Capabilities() scm.Capabilities {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.capabilities
}

// MaxPRBodyChars reports the description limit the plugin declared, in
// scm.PRBodyLen units; zero means no limit.
func (h *Host) MaxPRBodyChars() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxBodyChars
}

// Available runs the plugin's "status" handshake. A plugin that exits
// non-zero (not authenticated, cannot serve the repository) returns an
// ordinary error and the PR and CI steps skip with the plugin's own message.
// A handshake that breaks the contract (bad output, wrong protocol version,
// timeout) returns ErrProtocol and the steps fail closed.
func (h *Host) Available(ctx context.Context) error {
	if h.opts.Name == "" {
		return errors.New("provider plugin has no name")
	}
	if h.opts.Executable == "" {
		return fmt.Errorf("provider plugin %q has no command configured", h.opts.Name)
	}
	if h.opts.CommandFactory == nil {
		return fmt.Errorf("provider plugin %q has no command runner", h.opts.Name)
	}
	if h.opts.ExecutableAvailable != nil && !h.opts.ExecutableAvailable(h.opts.Executable) {
		return fmt.Errorf("provider plugin %q: command %q not found", h.opts.Name, h.opts.Executable)
	}
	var result statusResult
	if err := h.call(ctx, CmdStatus, nil, nil, &result, maxResponseBytes); err != nil {
		return err
	}
	if result.ProtocolVersion != ProtocolVersion {
		return h.protocolf(CmdStatus, "speaks protocol version %d; this no-mistakes requires version %d", result.ProtocolVersion, ProtocolVersion)
	}
	if result.MaxPRBodyChars == nil {
		return h.protocolf(CmdStatus, "response did not include max_pr_body_chars (use 0 for no limit)")
	}
	if *result.MaxPRBodyChars < 0 {
		return h.protocolf(CmdStatus, "declared a negative max_pr_body_chars")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.capabilities = scm.Capabilities{
		MergeableState:  result.Capabilities.MergeableState,
		FailedCheckLogs: result.Capabilities.FailedCheckLogs,
		MergedProof:     result.Capabilities.MergedProof,
	}
	h.canRetarget = result.Capabilities.SetPRBaseBranch
	h.maxBodyChars = *result.MaxPRBodyChars
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{flag("head", branch)}
	if base != "" {
		args = append(args, flag("base", base))
	}
	var result findResult
	if err := h.call(ctx, CmdPRFind, args, nil, &result, maxResponseBytes); err != nil {
		return nil, err
	}
	if len(result.PR) == 0 {
		return nil, h.protocolf(CmdPRFind, `response did not include "pr" (use null when there is no open PR)`)
	}
	if bytes.Equal(result.PR, []byte("null")) {
		return nil, nil
	}
	var found wirePR
	if err := json.Unmarshal(result.PR, &found); err != nil {
		return nil, h.protocolf(CmdPRFind, "returned an invalid pr: %v", err)
	}
	pr, err := h.normalizePR(CmdPRFind, &found)
	if err != nil {
		return nil, err
	}
	if found.HeadBranch != branch {
		return nil, h.protocolf(CmdPRFind, "returned PR %s for head branch %q, expected %q", pr.Number, found.HeadBranch, branch)
	}
	if base != "" && found.BaseBranch != base {
		return nil, h.protocolf(CmdPRFind, "returned PR %s targeting %q, expected %q", pr.Number, found.BaseBranch, base)
	}
	return pr, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	if err := h.checkBodyBudget(CmdPRCreate, content.Body); err != nil {
		return nil, err
	}
	args := []string{flag("head", branch), flag("base", base), flag("title", content.Title)}
	if h.opts.DraftPullRequests {
		args = append(args, "--draft")
	}
	var result prResult
	if err := h.call(ctx, CmdPRCreate, args, &content.Body, &result, maxResponseBytes); err != nil {
		return nil, err
	}
	pr, err := h.normalizePR(CmdPRCreate, result.PR)
	if err != nil {
		return nil, err
	}
	if result.PR.HeadBranch != branch || result.PR.BaseBranch != base {
		return nil, h.protocolf(CmdPRCreate, "created PR %s for %q -> %q, expected %q -> %q", pr.Number, result.PR.HeadBranch, result.PR.BaseBranch, branch, base)
	}
	return pr, nil
}

// UpdatePR replaces the PR description. An empty title omits --title, which
// means "leave the title unchanged"; the attestation rebind relies on that to
// avoid overwriting a concurrent title edit.
func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	number, err := h.prNumberOf(CmdPRUpdate, pr)
	if err != nil {
		return nil, err
	}
	if err := h.checkBodyBudget(CmdPRUpdate, content.Body); err != nil {
		return nil, err
	}
	args := []string{number}
	if content.Title != "" {
		args = append(args, flag("title", content.Title))
	}
	var result prResult
	if err := h.call(ctx, CmdPRUpdate, args, &content.Body, &result, maxResponseBytes); err != nil {
		return nil, err
	}
	updated, err := h.normalizePR(CmdPRUpdate, result.PR)
	if err != nil {
		return nil, err
	}
	if err := h.samePR(CmdPRUpdate, pr, number, updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	result, _, err := h.viewPR(ctx, pr)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(result.State)) {
	case "open":
		return scm.PRStateOpen, nil
	case "merged":
		return scm.PRStateMerged, nil
	case "closed":
		return scm.PRStateClosed, nil
	default:
		return "", h.protocolf(CmdPRView, "returned unknown PR state %q (want open, merged, or closed)", result.State)
	}
}

// GetPRContent reads the live title and raw body. Both fields must be present
// in the response: an explicitly empty body is valid, a missing or null one is
// not, because author-preserving publication would otherwise overwrite text it
// never saw.
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	result, _, err := h.viewPR(ctx, pr)
	if err != nil {
		return scm.PRContent{}, err
	}
	if result.Title == nil || result.Body == nil {
		return scm.PRContent{}, h.protocolf(CmdPRView, "response must include both title and body (body may be an empty string)")
	}
	return scm.PRContent{Title: *result.Title, Body: *result.Body}, nil
}

func (h *Host) GetPRBaseBranch(ctx context.Context, pr *scm.PR) (string, error) {
	_, live, err := h.viewPR(ctx, pr)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(live.BaseBranch) == "" {
		return "", h.protocolf(CmdPRView, "response did not include the PR's base_branch")
	}
	return live.BaseBranch, nil
}

func (h *Host) viewPR(ctx context.Context, pr *scm.PR) (viewResult, *scm.PR, error) {
	number, err := h.prNumberOf(CmdPRView, pr)
	if err != nil {
		return viewResult{}, nil, err
	}
	var result viewResult
	if err := h.call(ctx, CmdPRView, []string{number}, nil, &result, maxResponseBytes); err != nil {
		return viewResult{}, nil, err
	}
	live, err := h.normalizePR(CmdPRView, result.PR)
	if err != nil {
		return viewResult{}, nil, err
	}
	if err := h.samePR(CmdPRView, pr, number, live); err != nil {
		return viewResult{}, nil, err
	}
	return result, live, nil
}

// GetChecks returns the checks for the PR's current head on the forge. The
// "checks" key is required: an empty list is a real "nothing registered yet"
// answer, a missing one is an unreadable response, and CI readiness must never
// mistake the second for the first.
func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	number, err := h.prNumberOf(CmdPRChecks, pr)
	if err != nil {
		return nil, err
	}
	args := []string{number}
	if headSHA := strings.TrimSpace(pr.HeadSHA); headSHA != "" {
		args = append(args, flag("head-sha", headSHA))
	}
	var result checksResult
	if err := h.call(ctx, CmdPRChecks, args, nil, &result, maxResponseBytes); err != nil {
		return nil, err
	}
	if result.Checks == nil {
		return nil, h.protocolf(CmdPRChecks, "response did not include a checks list")
	}
	checks := make([]scm.Check, 0, len(*result.Checks))
	for i, raw := range *result.Checks {
		check, err := normalizeCheck(raw)
		if err != nil {
			return nil, h.protocolf(CmdPRChecks, "check %d: %v", i, err)
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func normalizeCheck(raw wireCheck) (scm.Check, error) {
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return scm.Check{}, errors.New("name is required")
	}
	check := scm.Check{
		Name:        name,
		ProviderID:  strings.TrimSpace(raw.ID),
		Kind:        scm.CheckKindRun,
		State:       strings.TrimSpace(raw.State),
		ExecutionID: strings.TrimSpace(raw.ExecutionID),
		Link:        strings.TrimSpace(raw.Link),
	}
	switch bucket := scm.CheckBucket(strings.ToLower(strings.TrimSpace(raw.Bucket))); bucket {
	case scm.CheckBucketPass, scm.CheckBucketFail, scm.CheckBucketPending, scm.CheckBucketCancel, scm.CheckBucketSkip:
		check.Bucket = bucket
	default:
		return scm.Check{}, fmt.Errorf("check %q has unknown bucket %q (want pass, fail, pending, cancel, or skipping)", name, raw.Bucket)
	}
	var err error
	if check.StartedAt, err = parseOptionalTime(raw.StartedAt); err != nil {
		return scm.Check{}, fmt.Errorf("check %q started_at: %w", name, err)
	}
	if check.CompletedAt, err = parseOptionalTime(raw.CompletedAt); err != nil {
		return scm.Check{}, fmt.Errorf("check %q completed_at: %w", name, err)
	}
	return check, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	if !h.Capabilities().MergeableState {
		return scm.MergeableUnknown, scm.ErrUnsupported
	}
	number, err := h.prNumberOf(CmdPRMergeability, pr)
	if err != nil {
		return scm.MergeableUnknown, err
	}
	var result mergeabilityResult
	if err := h.call(ctx, CmdPRMergeability, []string{number}, nil, &result, maxResponseBytes); err != nil {
		return scm.MergeableUnknown, err
	}
	switch strings.ToLower(strings.TrimSpace(result.State)) {
	case "mergeable":
		return scm.MergeableOK, nil
	case "conflicting":
		return scm.MergeableConflict, nil
	case "pending":
		return scm.MergeablePending, nil
	case "unknown":
		return scm.MergeableUnknown, nil
	default:
		return scm.MergeableUnknown, h.protocolf(CmdPRMergeability, "returned unknown state %q (want mergeable, conflicting, pending, or unknown)", result.State)
	}
}

// FetchFailedCheckLogs returns the plugin's failed-job logs, keeping the last
// 1 MiB when the plugin returns more: the end of a log is where a build or
// test failure is reported.
func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	if !h.Capabilities().FailedCheckLogs {
		return "", scm.ErrUnsupported
	}
	if len(failingNames) == 0 {
		return "", nil
	}
	number, err := h.prNumberOf(CmdPRCheckLogs, pr)
	if err != nil {
		return "", err
	}
	args := []string{number, "--failed"}
	if branch != "" {
		args = append(args, flag("branch", branch))
	}
	if headSHA != "" {
		args = append(args, flag("head-sha", headSHA))
	}
	for _, name := range failingNames {
		args = append(args, flag("check", name))
	}
	var result checkLogsResult
	if err := h.call(ctx, CmdPRCheckLogs, args, nil, &result, maxLogsResponseBytes); err != nil {
		return "", err
	}
	if result.Logs == nil {
		return "", h.protocolf(CmdPRCheckLogs, "response did not include logs (use an empty string when there are none)")
	}
	return keepTail(*result.Logs, maxLogsBytes), nil
}

func (h *Host) GetMergedProof(ctx context.Context, pr *scm.PR, expectedHead string) (scm.MergedProof, error) {
	if !h.Capabilities().MergedProof {
		return scm.MergedProof{}, scm.ErrUnsupported
	}
	number, err := h.prNumberOf(CmdPRMerged, pr)
	if err != nil {
		return scm.MergedProof{}, err
	}
	expectedHead = strings.TrimSpace(expectedHead)
	if expectedHead == "" {
		return scm.MergedProof{}, fmt.Errorf("provider plugin %q %s: merged proof requires an expected head SHA", h.opts.Name, CmdPRMerged)
	}
	var result mergedResult
	if err := h.call(ctx, CmdPRMerged, []string{number, flag("expected-head", expectedHead)}, nil, &result, maxResponseBytes); err != nil {
		return scm.MergedProof{}, err
	}
	if err := h.sameNumber(CmdPRMerged, number, string(result.Number)); err != nil {
		return scm.MergedProof{}, err
	}
	if wantURL := strings.TrimSpace(pr.URL); result.URL != wantURL {
		return scm.MergedProof{}, h.protocolf(CmdPRMerged, "proof identifies %q, expected %q", result.URL, wantURL)
	}
	if result.HeadSHA != expectedHead {
		return scm.MergedProof{}, fmt.Errorf("provider plugin %q %s: %w: expected %s, got %q", h.opts.Name, CmdPRMerged, scm.ErrHeadChanged, expectedHead, result.HeadSHA)
	}
	proof := scm.MergedProof{
		Merged:         result.Merged,
		Number:         string(result.Number),
		URL:            result.URL,
		HeadSHA:        result.HeadSHA,
		MergeCommitSHA: strings.TrimSpace(result.MergeCommitSHA),
		MergedBy:       strings.TrimSpace(result.MergedBy),
	}
	if proof.MergedAt, err = parseOptionalTime(result.MergedAt); err != nil {
		return scm.MergedProof{}, h.protocolf(CmdPRMerged, "merged_at: %v", err)
	}
	if proof.Merged && (proof.MergeCommitSHA == "" || proof.MergedAt.IsZero()) {
		return scm.MergedProof{}, h.protocolf(CmdPRMerged, "a merged proof must include merge_commit_sha and merged_at")
	}
	return proof, nil
}

// SetPRBaseBranch retargets an existing PR. A plugin that did not declare the
// capability fails closed, which is what the PR step requires of any host
// that cannot move a live base.
func (h *Host) SetPRBaseBranch(ctx context.Context, pr *scm.PR, baseBranch string) error {
	h.mu.Lock()
	canRetarget := h.canRetarget
	h.mu.Unlock()
	if !canRetarget {
		return fmt.Errorf("provider plugin %q cannot change a pull request's base branch: %w", h.opts.Name, scm.ErrUnsupported)
	}
	number, err := h.prNumberOf(CmdPRRetarget, pr)
	if err != nil {
		return err
	}
	var result prResult
	if err := h.call(ctx, CmdPRRetarget, []string{number, flag("base", baseBranch)}, nil, &result, maxResponseBytes); err != nil {
		return err
	}
	updated, err := h.normalizePR(CmdPRRetarget, result.PR)
	if err != nil {
		return err
	}
	if err := h.samePR(CmdPRRetarget, pr, number, updated); err != nil {
		return err
	}
	if updated.BaseBranch != baseBranch {
		return h.protocolf(CmdPRRetarget, "PR %s now targets %q, expected %q", updated.Number, updated.BaseBranch, baseBranch)
	}
	return nil
}

// checkBodyBudget refuses a description over the plugin's declared limit
// before anything is written. The PR step fits bodies under the limit, so
// reaching this is a no-mistakes bug, not a plugin protocol violation.
func (h *Host) checkBodyBudget(command, body string) error {
	if limit := h.MaxPRBodyChars(); limit > 0 && scm.PRBodyLen(body) > limit {
		return fmt.Errorf("provider plugin %q %s: description is %d characters, over the plugin's declared max_pr_body_chars %d", h.opts.Name, command, scm.PRBodyLen(body), limit)
	}
	return nil
}

// prNumberOf returns the PR number to pass as the positional argument. It
// must be a positive base-10 integer, so it can never be read as a flag.
func (h *Host) prNumberOf(command string, pr *scm.PR) (string, error) {
	if pr == nil {
		return "", fmt.Errorf("provider plugin %q %s: no pull request identity to send", h.opts.Name, command)
	}
	number := strings.TrimSpace(pr.Number)
	var parsed prNumber
	if number == "" || parsed.UnmarshalJSON([]byte(`"`+number+`"`)) != nil {
		return "", fmt.Errorf("provider plugin %q %s: no pull request identity to send (number %q)", h.opts.Name, command, pr.Number)
	}
	return number, nil
}

func (h *Host) sameNumber(command, want, got string) error {
	if got != want {
		return h.protocolf(command, "returned PR %q, expected PR %q", got, want)
	}
	return nil
}

// samePR checks that a read or write answered for the PR it was asked about:
// the same number and, when the caller holds one (a persisted run URL or the
// PR a lookup returned), the same URL. A number alone is not an identity
// across repositories, and a changed URL would be persisted by the run.
func (h *Host) samePR(command string, requested *scm.PR, number string, got *scm.PR) error {
	if err := h.sameNumber(command, number, got.Number); err != nil {
		return err
	}
	if want := strings.TrimSpace(requested.URL); want != "" && got.URL != want {
		return h.protocolf(command, "returned PR url %q, expected %q", got.URL, want)
	}
	return nil
}

// normalizePR validates a plugin-reported PR. The URL must end in the PR
// number because runs persist only the URL and later steps (CI, recovery,
// approval reconciliation) recover the number from it, and it must name the
// run's repository: its path must contain the repository path sent as
// --repo as consecutive segments. The host is not compared, because a
// forge's web host need not be its git host.
func (h *Host) normalizePR(command string, raw *wirePR) (*scm.PR, error) {
	if raw == nil {
		return nil, h.protocolf(command, "response did not include a pr object")
	}
	number := string(raw.Number)
	if number == "" {
		return nil, h.protocolf(command, "PR number is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(raw.URL))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return nil, h.protocolf(command, "PR %s url %q must be an http(s) URL without credentials", number, raw.URL)
	}
	if fromURL, err := scm.ExtractPRNumber(raw.URL); err != nil || fromURL != number {
		return nil, h.protocolf(command, "PR url %q must end with the PR number %s", raw.URL, number)
	}
	if !urlNamesRepository(parsed, h.opts.Repository.Path) {
		return nil, h.protocolf(command, "PR url %q does not name repository %q", raw.URL, h.opts.Repository.Path)
	}
	return &scm.PR{
		Number:     number,
		URL:        strings.TrimSpace(raw.URL),
		HeadSHA:    strings.TrimSpace(raw.HeadSHA),
		BaseBranch: strings.TrimSpace(raw.BaseBranch),
	}, nil
}

// urlNamesRepository reports whether the URL path contains the repository
// path as whole, consecutive segments (case-insensitively, as forges treat
// owner and repository names). An unknown repository path proves nothing,
// so it never matches.
func urlNamesRepository(u *url.URL, repoPath string) bool {
	repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
	if repoPath == "" {
		return false
	}
	path := strings.ToLower("/" + strings.Trim(u.Path, "/") + "/")
	return strings.Contains(path, strings.ToLower("/"+repoPath+"/"))
}

// invocation is the raw outcome of one plugin process.
type invocation struct {
	stdout   []byte
	stderr   string
	exceeded bool
	runErr   error
}

// call runs one plugin invocation and decodes its result into dst. body, when
// non-nil, is written to a private temporary file passed as --body-file and
// removed when the call returns.
func (h *Host) call(parent context.Context, command string, args []string, body *string, dst any, maxStdout int) error {
	argv := append(append([]string(nil), h.opts.Args...), commandWords(command)...)
	argv = append(argv, args...)
	if body != nil {
		path, cleanup, err := writeBodyFile(h.opts.BodyDir, *body)
		if err != nil {
			return fmt.Errorf("provider plugin %q %s: %w", h.opts.Name, command, err)
		}
		defer cleanup()
		argv = append(argv, flag("body-file", path))
	}
	argv = append(argv, contextArgs(h.opts.Name, h.opts.Repository)...)
	argv = append(argv, "--json")

	ctx, cancel := context.WithTimeout(parent, h.opts.Timeout)
	defer cancel()
	run := h.run
	if run == nil {
		run = h.runProcess
	}
	out, err := run(ctx, argv, maxStdout)
	if err != nil {
		return fmt.Errorf("provider plugin %q %s: %w", h.opts.Name, command, err)
	}
	if err := parent.Err(); err != nil {
		return err
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &protocolError{plugin: h.opts.Name, command: command, message: fmt.Sprintf("timed out after %s", h.opts.Timeout), timeout: true}
	}
	if out.exceeded {
		return h.protocolf(command, "output exceeded %d bytes", maxStdout)
	}
	if out.runErr != nil {
		if reported := decodeFailure(out.stdout); reported != nil {
			return &Error{Plugin: h.opts.Name, Operation: command, Code: strings.TrimSpace(reported.Code), Message: boundedText(reported.Message)}
		}
		detail := boundedText(out.stderr)
		if detail == "" {
			return fmt.Errorf("provider plugin %q %s failed: %w", h.opts.Name, command, out.runErr)
		}
		return fmt.Errorf("provider plugin %q %s failed: %w: %s", h.opts.Name, command, out.runErr, detail)
	}
	if err := decodeResult(out.stdout, dst); err != nil {
		return h.protocolf(command, "%v", err)
	}
	return nil
}

// writeBodyFile writes a PR description to a fresh 0600 file (os.CreateTemp's
// mode) that only the daemon's user can read.
func writeBodyFile(dir, body string) (string, func(), error) {
	f, err := os.CreateTemp(dir, "no-mistakes-pr-body-*.md")
	if err != nil {
		return "", nil, fmt.Errorf("create PR body file: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write PR body file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close PR body file: %w", err)
	}
	return path, cleanup, nil
}

// protocolf reports a contract violation. It matches ErrProtocol.
func (h *Host) protocolf(command, format string, args ...any) error {
	return &protocolError{plugin: h.opts.Name, command: command, message: fmt.Sprintf(format, args...)}
}

type protocolError struct {
	plugin, command, message string
	timeout                  bool
}

func (e *protocolError) Error() string {
	return fmt.Sprintf("provider plugin %q %s: %s", e.plugin, e.command, e.message)
}

func (e *protocolError) Is(target error) bool {
	return target == ErrProtocol || (e.timeout && target == ErrTimeout)
}

// runProcess spawns the configured executable once with stdin empty. Stdout
// is capped (a plugin that floods it fails the call) while stderr keeps only
// its first bytes and never fails the write, so a chatty plugin is never
// killed by SIGPIPE.
func (h *Host) runProcess(ctx context.Context, args []string, maxStdout int) (invocation, error) {
	if h.opts.CommandFactory == nil {
		return invocation{}, errors.New("no command runner")
	}
	cmd := h.opts.CommandFactory(ctx, h.opts.Executable, args...)
	if cmd == nil {
		return invocation{}, errors.New("command runner returned no command")
	}
	shellenv.ConfigureShellCommand(cmd)
	stdout := &cappedBuffer{limit: maxStdout}
	stderr := &prefixBuffer{limit: maxStderrBytes}
	// A nil Stdin is the null device: the plugin reads EOF immediately.
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := shellenv.RunShellCommand(cmd)
	return invocation{stdout: stdout.Bytes(), stderr: stderr.String(), exceeded: stdout.exceeded, runErr: runErr}, nil
}

// Error is a failure the plugin reported through a failure document on a
// non-zero exit.
type Error struct {
	Plugin    string
	Operation string
	Code      string
	Message   string
}

func (e *Error) Error() string {
	message := e.Message
	if message == "" {
		message = "no message"
	}
	if e.Code != "" {
		return fmt.Sprintf("provider plugin %q %s: %s (%s)", e.Plugin, e.Operation, message, e.Code)
	}
	return fmt.Sprintf("provider plugin %q %s: %s", e.Plugin, e.Operation, message)
}

// Unwrap maps protocol error codes onto the sentinels callers branch on. See
// ErrorCodeUnsupported for why "unsupported" is a protocol violation.
func (e *Error) Unwrap() error {
	switch e.Code {
	case ErrorCodeUnsupported:
		return ErrProtocol
	case ErrorCodeHeadChanged:
		return scm.ErrHeadChanged
	default:
		return nil
	}
}

func parseOptionalTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

// boundedText redacts credentials and caps plugin-supplied text so a noisy
// plugin cannot flood step logs or the IPC event stream.
func boundedText(text string) string {
	text = strings.TrimSpace(safeurl.RedactText(text))
	if len(text) <= maxErrorMessageBytes {
		return text
	}
	cut := maxErrorMessageBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…(truncated)"
}

func keepTail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const marker = "…(log truncated)\n"
	start := len(text) - (limit - len(marker))
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return marker + text[start:]
}

type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.exceeded = true
		return 0, errors.New("provider plugin output limit exceeded")
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.exceeded = true
		return remaining, errors.New("provider plugin output limit exceeded")
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// prefixBuffer keeps the first limit bytes of stderr and discards the rest
// without failing the write, so a chatty plugin is never killed by SIGPIPE.
type prefixBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *prefixBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *prefixBuffer) String() string { return b.buf.String() }

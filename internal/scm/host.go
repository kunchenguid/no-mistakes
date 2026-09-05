package scm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ExtractHost returns the lowercased host (without any port) from a git
// remote URL. It handles both scp-like syntax (git@host:group/project) and
// URL forms (https://host/group/project, ssh://git@host:22/group/project).
// It returns "" when no host can be determined.
func ExtractHost(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		// URL form: scheme://[user@]host[:port]/path. Split off the path at the
		// first '/' before scanning for userinfo, so a '@' inside the path
		// (e.g. .../group@prod/repo.git) cannot be mistaken for a "user@" prefix.
		s = s[i+3:]
		if slash := strings.Index(s, "/"); slash >= 0 {
			s = s[:slash]
		}
		if at := strings.LastIndex(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		return strings.ToLower(stripPort(s))
	}
	// No scheme. scp-like syntax is [user@]host:path; the first ':' separates
	// the host from the path. Split off the path first, then strip any userinfo
	// prefix from the host segment only, so a '@' in the path (e.g.
	// git@host:group@prod/repo.git) cannot collapse host extraction.
	if c := strings.Index(s, ":"); c >= 0 {
		s = s[:c]
	} else if slash := strings.Index(s, "/"); slash >= 0 {
		s = s[:slash]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	return strings.ToLower(s)
}

// stripPort removes a trailing :port from a host, leaving bare hosts and
// bracketed IPv6 literals intact.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		// IPv6 literal: [::1]:22 -> [::1]
		if end := strings.Index(host, "]"); end >= 0 {
			return host[:end+1]
		}
		return host
	}
	if c := strings.LastIndex(host, ":"); c >= 0 {
		port := host[c+1:]
		if port != "" && strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			return host[:c]
		}
	}
	return host
}

// ExtractPRNumber returns the trailing numeric segment from a PR/MR URL.
// Supports GitHub (/pull/N), GitLab (/-/merge_requests/N), Forgejo
// (/pulls/N), Bitbucket (/pull-requests/N), and Azure DevOps (/pullrequest/N)
// URLs; all of them end in a digit path segment.
func ExtractPRNumber(prURL string) (string, error) {
	trimmed := strings.TrimRight(prURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	num := parts[len(parts)-1]
	if num == "" {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	if _, err := strconv.Atoi(num); err != nil {
		return "", fmt.Errorf("invalid PR number %q in URL: %s", num, prURL)
	}
	return num, nil
}

// PR identifies a pull/merge request on a provider.
type PR struct {
	Number string
	URL    string
	// HeadSHA scopes provider check discovery to the exact commit currently
	// being certified. Providers that expose CI outside the PR check rollup
	// use it to include those runs.
	HeadSHA string
	// BaseBranch is the forge's actual target branch for this PR. It is
	// authoritative once a PR exists and protects resumed CI repair from a
	// later configuration change.
	BaseBranch string
}

// PRContent is the title + body for creating or updating a PR.
type PRContent struct {
	Title string
	Body  string
}

// PRState is the normalized lifecycle state of a PR.
type PRState string

const (
	PRStateOpen   PRState = "OPEN"
	PRStateMerged PRState = "MERGED"
	PRStateClosed PRState = "CLOSED"
)

// MergeableState is the normalized merge-conflict status of a PR.
type MergeableState string

const (
	MergeableOK       MergeableState = "MERGEABLE"
	MergeableConflict MergeableState = "CONFLICTING"
	MergeablePending  MergeableState = "PENDING"
	MergeableUnknown  MergeableState = "UNKNOWN"
)

// Conflict reports whether the state indicates a known merge conflict.
func (s MergeableState) Conflict() bool { return s == MergeableConflict }

// Resolved reports whether the state is final (MERGEABLE or CONFLICTING).
func (s MergeableState) Resolved() bool {
	return s == MergeableOK || s == MergeableConflict
}

// CheckBucket is the normalized outcome of a CI check.
type CheckBucket string

const (
	CheckBucketPass    CheckBucket = "pass"
	CheckBucketFail    CheckBucket = "fail"
	CheckBucketPending CheckBucket = "pending"
	CheckBucketCancel  CheckBucket = "cancel"
	CheckBucketSkip    CheckBucket = "skipping"
)

type CheckKind string

const (
	CheckKindRun    CheckKind = "run"
	CheckKindStatus CheckKind = "status"
)

// Check is a single CI check result on a PR.
type Check struct {
	Name   string
	Bucket CheckBucket
	Kind   CheckKind
	// State is the provider's own outcome string for the check (GitHub
	// conclusions such as FAILURE, TIMED_OUT, CANCELLED). Buckets collapse
	// several outcomes into one value, so callers that must tell an
	// infrastructure outcome from a real job failure read this. Empty when the
	// provider reported no state.
	State       string
	CompletedAt time.Time // zero when unknown; used to detect CI re-runs between polls
	// StartedAt is when this specific check run began. It is the ordering key
	// backends use to collapse superseded same-name check runs (e.g. a raw
	// commit rollup that keeps every run a commit ever had) down to the
	// latest one; zero when the provider did not report it.
	StartedAt time.Time
	// WorkflowID identifies the provider workflow that emitted the check. It
	// distinguishes independent same-name workflows while allowing reruns of
	// one workflow to use latest-wins ordering. Zero when unavailable.
	WorkflowID int64
	// Link is the provider's details URL for this check. It may identify an
	// individual job or a provider-side workflow run for targeted reruns. Empty
	// when the provider reported no link.
	Link string
	// PreRunFailure marks a check the provider failed before the repository's own
	// steps ran - its setup/action-resolution phase failed (e.g. a GitHub Actions
	// action-download outage), so no repository step executed. It is an
	// infrastructure outcome, not a verdict on the code, and the CI step treats it
	// as re-runnable rather than a code failure. A PreRunFailureDetector sets it;
	// it can never be true for a genuine test or lint failure, whose job cleared
	// setup and failed a later step.
	PreRunFailure bool
}

// Failing reports whether the check is in a failed bucket.
func (c Check) Failing() bool { return c.Bucket == CheckBucketFail }

// Pending reports whether the check is still running or queued.
func (c Check) Pending() bool { return c.Bucket == CheckBucketPending }

// Capabilities declares which optional Host methods return meaningful data.
// Callers must consult Capabilities before invoking optional methods.
type Capabilities struct {
	MergeableState  bool
	FailedCheckLogs bool
	MergedProof     bool
	ReviewComments  bool
}

var (
	// ErrUnsupported is returned by optional Host methods that the provider
	// cannot fulfil. Callers should gate calls on Capabilities rather than
	// relying on this error, but implementations return it as a fallback.
	ErrUnsupported = errors.New("operation not supported by this provider")
	// ErrHeadChanged rejects results for a different PR head than the run is
	// monitoring. It prevents a late status or already-merged race from proving
	// the wrong commit.
	ErrHeadChanged = errors.New("pull request head changed")
)

// ReviewComment represents a code review comment or bot finding on a pull request.
type ReviewComment struct {
	ID        string
	Author    string
	Path      string
	Line      int
	Body      string
	CreatedAt time.Time
	URL       string
}

// ReviewCommentsHost is an optional interface for SCM hosts that support fetching
// unresolved review comments on a pull request.
type ReviewCommentsHost interface {
	GetReviewComments(ctx context.Context, pr *PR) ([]ReviewComment, error)
}

// PRContentReader is an optional interface for hosts that can read the current
// title and body of an existing PR. The CI repair publisher uses it to rebind
// a live pipeline attestation to a newly published head without rewriting the
// rest of the body or inventing an attestation that was not already there.
type PRContentReader interface {
	GetPRContent(ctx context.Context, pr *PR) (PRContent, error)
}

// MergedProof is provider evidence that a specific PR head was merged.
type MergedProof struct {
	Merged         bool
	Number         string
	URL            string
	HeadSHA        string
	MergeCommitSHA string
	MergedAt       time.Time
	MergedBy       string
}

// MergedProofHost is implemented by hosts that can prove which exact PR head
// was merged. The expected head must be checked even when the PR is already
// merged, because merge and monitor polling can race.
type MergedProofHost interface {
	GetMergedProof(ctx context.Context, pr *PR, expectedHead string) (MergedProof, error)
}

// Host is the provider-agnostic interface to a PR-hosting service.
// Transport (CLI vs HTTP API) is an implementation detail.
type Host interface {
	Provider() Provider
	Capabilities() Capabilities

	// Available returns nil when the host is ready to use, or a descriptive
	// error explaining why it is not (missing CLI, unauthenticated, etc).
	Available(ctx context.Context) error

	// FindPR returns the open PR for the source branch, or nil only when a
	// successfully decoded and validated PR listing contains no matching PR. It
	// returns an error for lookup, response-decoding, or validation failures
	// (including empty, malformed, null, or incoherent payloads) so callers do
	// not create a duplicate PR after an indeterminate lookup.
	FindPR(ctx context.Context, branch, base string) (*PR, error)
	CreatePR(ctx context.Context, branch, base string, content PRContent) (*PR, error)
	UpdatePR(ctx context.Context, pr *PR, content PRContent) (*PR, error)

	GetPRState(ctx context.Context, pr *PR) (PRState, error)
	GetChecks(ctx context.Context, pr *PR) ([]Check, error)

	// GetMergeableState is optional; implementations without Capabilities().MergeableState
	// must return ErrUnsupported. Callers should consult Capabilities first.
	GetMergeableState(ctx context.Context, pr *PR) (MergeableState, error)

	// FetchFailedCheckLogs is optional; returns "" when no logs can be retrieved
	// and ErrUnsupported when the provider has no log-fetching support at all.
	FetchFailedCheckLogs(ctx context.Context, pr *PR, branch, headSHA string, failingNames []string) (string, error)
}

// PRBaseBranchReader is implemented by providers that can read the target
// branch of an existing PR by its durable identity. CI uses it when a run is
// resumed after repository configuration changes.
type PRBaseBranchReader interface {
	GetPRBaseBranch(ctx context.Context, pr *PR) (string, error)
}

// PRBaseRetargeter is implemented by providers that can change an existing
// PR's target branch. The PR step uses it when a per-run --base-branch override
// disagrees with the live forge base of an already-open PR. A host that does
// not implement this, including when the live base is unread, must fail closed
// rather than rewrite title and body against a base it did not move. A
// repo-config pr.base_branch change still does not retarget; that path updates
// title and body only so a still-open PR is not orphaned behind a duplicate.
type PRBaseRetargeter interface {
	SetPRBaseBranch(ctx context.Context, pr *PR, baseBranch string) error
}

// PreRunFailureDetector reports which failed checks the provider failed before
// the repository's own steps ran - a setup/action-resolution outcome (for GitHub
// Actions, an action-download outage) rather than a verdict on the code. It
// reads the provider's own step-level conclusions, never log text, so a flagged
// check is one whose job never executed a repository step. A genuine test or
// lint failure can never be flagged, because that job cleared setup and failed a
// later step: this is what keeps the transient-rerun path from masking real
// failures.
//
// Like CheckRerunner it is optional: a backend whose provider exposes no
// step-level phase simply does not implement it, and the CI step consults it
// only when transient reruns are enabled.
type PreRunFailureDetector interface {
	// PreRunFailures returns a slice parallel to checks: entry i is true when
	// checks[i] failed before any repository step ran. Check names are not unique
	// on a PR, so the result is positional rather than name-keyed - a same-named
	// genuine failure must never inherit another check's infrastructure flag. It
	// must fail closed - leaving false any check whose phase it cannot determine -
	// so an unreadable job stays a genuine failure rather than being masked as
	// infrastructure.
	PreRunFailures(ctx context.Context, checks []Check) ([]bool, error)
}

// CheckRerunner re-runs the provider-side work behind a failed check without
// changing the commit under test. It is deliberately a separate interface
// rather than a Host method: a backend whose provider exposes no rerun
// primitive simply does not implement it, and callers type-assert
// (host.(CheckRerunner)) before use, so those backends keep compiling and keep
// their existing behavior.
type CheckRerunner interface {
	// RerunCheck asks the provider to run check again for the same commit. It
	// returns an error when the request could not be made, including when the
	// check names no job or workflow run the provider can re-run.
	RerunCheck(ctx context.Context, pr *PR, check Check) error
}

// RepoPath extracts a repository path from a git remote or web URL. Nested
// namespaces are preserved. Azure DevOps remotes use project/repository.
func RepoPath(remoteURL string) string {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return ""
	}

	host := ExtractHost(raw)
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		raw = u.Path
	case strings.Contains(raw, ":"):
		colon := strings.IndexByte(raw, ':')
		if colon <= 0 || strings.Contains(raw[:colon], "/") {
			return ""
		}
		raw = raw[colon+1:]
	}

	parts := strings.Split(strings.Trim(raw, "/"), "/")
	clean := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			clean = append(clean, part)
		}
	}
	parts = clean
	if len(parts) == 0 {
		return ""
	}

	isAzureDevOps := host == "dev.azure.com" || host == "ssh.dev.azure.com" || strings.HasSuffix(host, ".visualstudio.com")
	if isAzureDevOps {
		for i, part := range parts {
			if strings.EqualFold(part, "_git") && i > 0 && i+1 < len(parts) {
				return parts[i-1] + "/" + strings.TrimSuffix(parts[i+1], ".git")
			}
		}
	}
	if (host == "ssh.dev.azure.com" || host == "vs-ssh.visualstudio.com") && len(parts) >= 4 && strings.EqualFold(parts[0], "v3") {
		return parts[len(parts)-2] + "/" + strings.TrimSuffix(parts[len(parts)-1], ".git")
	}
	parts[len(parts)-1] = strings.TrimSuffix(parts[len(parts)-1], ".git")
	return strings.Join(parts, "/")
}

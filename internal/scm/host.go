package scm

import (
	"context"
	"errors"
	"fmt"
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
	Number  string
	URL     string
	HeadSHA string // populated when the provider exposes the exact source commit
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

// Check is a single CI check result on a PR.
type Check struct {
	Name   string
	Bucket CheckBucket
	// State is the provider's own outcome string for the check (GitHub
	// conclusions such as FAILURE, TIMED_OUT, CANCELLED). Buckets collapse
	// several outcomes into one value, so callers that must tell an
	// infrastructure outcome from a real job failure read this. Empty when the
	// provider reported no state.
	State       string
	CompletedAt time.Time // zero when unknown; used to detect CI re-runs between polls
	// Link is the provider's details URL for this check. It identifies the job
	// behind the check, so a rerun can target that job instead of the whole PR.
	// Empty when the provider reported no link.
	Link string
}

// Failing reports whether the check is in a failed bucket.
func (c Check) Failing() bool { return c.Bucket == CheckBucketFail }

// Pending reports whether the check is still running or queued.
func (c Check) Pending() bool { return c.Bucket == CheckBucketPending }

// Capabilities declares which optional Host methods return meaningful data.
// Callers must consult Capabilities before invoking optional methods.
type Capabilities struct {
	MergeableState    bool
	FailedCheckLogs   bool
	MergedProof       bool
	PullRequests      bool
	CommitStatuses    bool
	BranchProtection  bool
	ExpectedHeadMerge bool
	ActionsJobLogs    bool
	ActionsRuns       bool
	ActionsRunJobs    bool
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

	// FindPR returns the open PR for the source branch, or nil if none exists.
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

// CheckRerunner re-runs the provider-side job behind a failed check without
// changing the commit under test. It is deliberately a separate interface
// rather than a Host method: a backend whose provider exposes no rerun
// primitive simply does not implement it, and callers type-assert
// (host.(CheckRerunner)) before use, so those backends keep compiling and keep
// their existing behavior.
type CheckRerunner interface {
	// RerunCheck asks the provider to run check again for the same commit. It
	// returns an error when the request could not be made, including when the
	// check names no job the provider can re-run.
	RerunCheck(ctx context.Context, pr *PR, check Check) error
}

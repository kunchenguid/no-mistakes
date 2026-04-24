package scm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ExtractPRNumber returns the trailing numeric segment from a PR/MR URL.
// Supports GitHub (/pull/N), GitLab (/-/merge_requests/N), and Bitbucket
// (/pull-requests/N) URLs; all of them end in a digit path segment.
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
	Name        string
	Bucket      CheckBucket
	CompletedAt time.Time // zero when unknown; used to detect CI re-runs between polls
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
}

// ErrUnsupported is returned by optional Host methods that the provider
// cannot fulfil. Callers should gate calls on Capabilities rather than
// relying on this error, but implementations return it as a fallback.
var ErrUnsupported = errors.New("operation not supported by this provider")

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

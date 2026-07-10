package agent

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// OperationalFailureKind classifies a terminal agent failure that, after
// adapter retries, reflects provider availability rather than the quality of
// the attempted change. Only these open a provider circuit (a later ticket);
// malformed output, poor patches, failed checks, and cancellation never do.
type OperationalFailureKind string

const (
	OpFailureQuota      OperationalFailureKind = "quota"
	OpFailureOutage     OperationalFailureKind = "outage"
	OpFailureOverload   OperationalFailureKind = "overload"
	OpFailureAuth       OperationalFailureKind = "auth"
	OpFailureExecutable OperationalFailureKind = "executable"
)

// OperationalError wraps a terminal agent error whose cause is a classified
// operational provider failure. It is produced only after adapter retries are
// exhausted; the original error remains available through Unwrap.
type OperationalError struct {
	Kind OperationalFailureKind
	Err  error
}

func (e *OperationalError) Error() string { return e.Err.Error() }
func (e *OperationalError) Unwrap() error { return e.Err }

// modelOutputError marks a failure caused by the model's own output — text
// that failed JSON/schema parsing, or a missing structured result — rather
// than provider availability. Such failures must never classify as
// operational, even when the model's text incidentally mentions a status code.
type modelOutputError struct{ err error }

func (e *modelOutputError) Error() string { return e.err.Error() }
func (e *modelOutputError) Unwrap() error { return e.err }

// markModelOutput tags err as a model-output failure. The visible message is
// unchanged; only classification provenance is added.
func markModelOutput(err error) error {
	if err == nil {
		return nil
	}
	return &modelOutputError{err: err}
}

// operationalNeedles maps case-insensitive substrings to a failure kind. Order
// matters: the most specific provider-availability signals are checked first so
// an executable-not-found never reads as a generic outage. HTTP status codes
// are matched separately (operationalStatus*) with word boundaries so a code
// never matches an incidental longer number such as 5000.
var operationalNeedles = []struct {
	needle string
	kind   OperationalFailureKind
}{
	{"executable file not found", OpFailureExecutable},
	{"file not found in $path", OpFailureExecutable},
	{"fork/exec", OpFailureExecutable},

	{"quota", OpFailureQuota},
	{"usage limit", OpFailureQuota},
	{"session limit", OpFailureQuota},
	{"insufficient_quota", OpFailureQuota},
	{"billing", OpFailureQuota},

	{"unauthorized", OpFailureAuth},
	{"authentication", OpFailureAuth},
	{"invalid api key", OpFailureAuth},
	{"invalid_api_key", OpFailureAuth},
	{"not authenticated", OpFailureAuth},

	{"overloaded", OpFailureOverload},
	{"rate limit", OpFailureOverload},
	{"rate_limit", OpFailureOverload},
	{"rate_limited", OpFailureOverload},
	{"too many requests", OpFailureOverload},

	{"service_unavailable", OpFailureOutage},
	{"service unavailable", OpFailureOutage},
	{"bad gateway", OpFailureOutage},
	{"gateway timeout", OpFailureOutage},
	{"internal server error", OpFailureOutage},
}

// HTTP status codes classified by word boundary, mapped to a failure kind.
var (
	operationalOverloadStatusRE = regexp.MustCompile(`\b429\b`)
	operationalOutageStatusRE   = regexp.MustCompile(`\b(500|502|503|504|529)\b`)
	operationalAuthStatusRE     = regexp.MustCompile(`\b(401|403)\b`)
)

// classifyOperationalFailure reports whether err is an operational provider
// failure and, if so, its kind. Cancellation and deadline errors are never
// operational so explicit cancellation cannot open a circuit.
func classifyOperationalFailure(err error) (OperationalFailureKind, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	var modelOutput *modelOutputError
	if errors.As(err, &modelOutput) {
		return "", false
	}
	msg := strings.ToLower(err.Error())
	for _, n := range operationalNeedles {
		if strings.Contains(msg, n.needle) {
			return n.kind, true
		}
	}
	switch {
	case operationalOverloadStatusRE.MatchString(msg):
		return OpFailureOverload, true
	case operationalOutageStatusRE.MatchString(msg):
		return OpFailureOutage, true
	case operationalAuthStatusRE.MatchString(msg):
		return OpFailureAuth, true
	}
	return "", false
}

// withOperationalClassification wraps a terminal error in an OperationalError
// when it classifies as an operational provider failure, and otherwise returns
// it unchanged. Adapters call it once after their retry loop so a genuine
// provider-availability failure carries a structured domain the circuit logic
// (a later ticket) can act on, without altering non-operational failures.
//
// A cancelled context never yields an OperationalError, even when the killed
// process's exit error incidentally carries a provider-looking message, so an
// aborted or superseded run is never mistaken for a provider outage.
func withOperationalClassification(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return err
	}
	if kind, ok := classifyOperationalFailure(err); ok {
		return &OperationalError{Kind: kind, Err: err}
	}
	return err
}

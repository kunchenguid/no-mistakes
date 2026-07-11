package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
)

// retryClassifier inspects an error and reports whether it should be retried,
// returning a short human-readable label for telemetry.
type retryClassifier func(error) (label string, retry bool)

// transientBackoff is the package-level sleep function used between retries.
// It is overridden in tests to keep them fast while preserving cancellation
// semantics.
var transientBackoff = func(ctx context.Context, attempt int) error {
	delay := transientBackoffBaseDuration(attempt, time.Second)
	// Apply +/- 25% jitter.
	if delay > 0 {
		span := int64(delay) / 2
		if span > 0 {
			//nolint:gosec // non-cryptographic jitter is fine here.
			delay += time.Duration(rand.Int64N(span+1)) - delay/4
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// transientBackoffBaseDuration returns the un-jittered delay for a given
// 1-indexed retry attempt. Progression: base, 4*base, 16*base, ...
func transientBackoffBaseDuration(attempt int, base time.Duration) time.Duration {
	if attempt < 1 {
		return 0
	}
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 4
	}
	return delay
}

// failedAttemptRestoreError preserves both failures in its message while
// unwrapping only to the restore failure. In particular, an operational
// provider error from the abandoned attempt must not authorize provider
// failover when the candidate itself could not be restored.
type failedAttemptRestoreError struct {
	attemptErr error
	restoreErr error
}

func (e *failedAttemptRestoreError) Error() string {
	return fmt.Sprintf("attempt failed (%v); restore failed: %v", e.attemptErr, e.restoreErr)
}

func (e *failedAttemptRestoreError) Unwrap() error { return e.restoreErr }

// RestoreFailedAttempt runs the idempotent candidate restore installed by the
// routing layer. Nil means either no isolation was required or restoration
// succeeded. A non-nil result is fatal and deliberately cannot unwrap to the
// abandoned attempt's OperationalError.
func RestoreFailedAttempt(opts RunOpts, attemptErr error) error {
	if attemptErr == nil || opts.AttemptIsolation == nil {
		return nil
	}
	if err := opts.AttemptIsolation.RestoreFailedAttempt(); err != nil {
		return &failedAttemptRestoreError{attemptErr: attemptErr, restoreErr: err}
	}
	return nil
}

// runWithRetry invokes runOnce up to maxRetries+1 times, retrying when the
// classifier marks the error as retriable. Between retries it sleeps with
// exponential backoff (via transientBackoff) and respects ctx cancellation.
// The retry attempt and classification label are surfaced to opts.OnLifecycle,
// falling back to opts.OnChunk for older direct callers.
func runWithRetry(
	ctx context.Context,
	name string,
	opts RunOpts,
	maxRetries int,
	classify retryClassifier,
	recoverRetry func(label string),
	runOnce func() (*Result, error),
) (*Result, error) {
	var lastErr error
	var lastLabel string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			emitAgentRetry(opts, name, lastLabel, attempt+1, maxRetries+1)
			if err := transientBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		startedAt := time.Now()
		result, err := runOnce()
		emitAgentAttempt(opts, name, result, err, startedAt, time.Now())
		if err == nil {
			return result, nil
		}
		if restoreErr := RestoreFailedAttempt(opts, err); restoreErr != nil {
			return nil, restoreErr
		}
		label, retry := classify(err)
		if !retry {
			return nil, err
		}
		if recoverRetry != nil {
			recoverRetry(label)
		}
		lastErr = err
		lastLabel = label
	}
	return nil, lastErr
}

func emitAgentAttempt(opts RunOpts, name string, result *Result, err error, startedAt, completedAt time.Time) {
	if opts.OnAttempt == nil {
		return
	}
	opts.OnAttempt(Attempt{
		Agent:           name,
		Result:          result,
		Err:             err,
		StartedAt:       startedAt,
		CompletedAt:     completedAt,
		Session:         cloneSessionRef(opts.Session),
		SessionFallback: opts.SessionFallback,
	})
}

func cloneSessionRef(session *SessionRef) *SessionRef {
	if session == nil {
		return nil
	}
	copy := *session
	return &copy
}

// claudeRetryClassifier retries both transient API errors and the
// no-structured-output case that the existing loop already handled.
func claudeRetryClassifier(err error) (string, bool) {
	if errors.Is(err, errNoStructuredOutput) {
		return "missing structured output", true
	}
	return classifyTransient(err)
}

var transientStatusRE = regexp.MustCompile(`\b(429|500|502|503|504|529)\b`)

// transientNeedles matches case-insensitive substrings emitted by Anthropic
// API errors, the various agent CLIs, or Go's net stack when the underlying
// failure is recoverable (load shed, network blip, DNS hiccup, etc.).
var transientNeedles = []struct {
	needle string
	label  string
}{
	{"overloaded_error", "overloaded_error"},
	{`"type":"overloaded"`, "overloaded_error"},
	{"rate_limit_error", "rate_limit_error"},
	{"rate_limited", "rate_limited"},
	{"service_unavailable", "service_unavailable"},
	{"connection refused", "connection refused"},
	{"connection reset", "connection reset"},
	{"i/o timeout", "i/o timeout"},
	{"no such host", "dns lookup failed"},
	{"temporary failure in name resolution", "dns temporary failure"},
	{"tls handshake", "tls handshake failure"},
	{"unexpected eof", "unexpected eof"},
	// Overload and outage signals that the operational classifier also treats
	// as provider-availability failures: they must be retried here before a
	// terminal operational classification, so classification happens only after
	// adapter retries are exhausted.
	{"overloaded", "overloaded"},
	{"too many requests", "too many requests"},
	{"bad gateway", "bad gateway"},
	{"gateway timeout", "gateway timeout"},
}

// classifyTransient reports whether an error message looks like a transient
// API or network failure. It deliberately ignores ctx cancellation/deadline
// errors so explicit cancellation is never silently retried.
func classifyTransient(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	// Model-output failures (malformed or schema-invalid text) are never a
	// transient provider signal, even when the model's text incidentally
	// contains an overload/outage word, so they are not retried.
	var modelOutput *modelOutputError
	if errors.As(err, &modelOutput) {
		return "", false
	}
	msg := strings.ToLower(err.Error())
	if m := transientStatusRE.FindString(msg); m != "" {
		return "http " + m, true
	}
	for _, sig := range transientNeedles {
		if strings.Contains(msg, sig.needle) {
			return sig.label, true
		}
	}
	return "", false
}

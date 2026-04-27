package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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
			delay += time.Duration(rand.Int63n(span+1)) - delay/4
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

// runWithRetry invokes runOnce up to maxRetries+1 times, retrying when the
// classifier marks the error as retriable. Between retries it sleeps with
// exponential backoff (via transientBackoff) and respects ctx cancellation.
// The retry attempt and classification label are surfaced to opts.OnChunk.
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
			if opts.OnChunk != nil {
				opts.OnChunk(fmt.Sprintf(
					"%s retrying after transient error %q (attempt %d/%d)",
					name, lastLabel, attempt+1, maxRetries+1,
				))
			}
			if err := transientBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		result, err := runOnce()
		if err == nil {
			return result, nil
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

// claudeRetryClassifier retries both transient API errors and the
// no-structured-output case that the existing loop already handled.
func claudeRetryClassifier(err error) (string, bool) {
	if errors.Is(err, errNoStructuredOutput) {
		return "missing structured output", true
	}
	return classifyTransient(err)
}

var transientStatusRE = regexp.MustCompile(`\b(429|503|529)\b`)

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
	msg := strings.ToLower(err.Error())
	for _, sig := range transientNeedles {
		if strings.Contains(msg, sig.needle) {
			return sig.label, true
		}
	}
	if m := transientStatusRE.FindString(msg); m != "" {
		return "http " + m, true
	}
	return "", false
}

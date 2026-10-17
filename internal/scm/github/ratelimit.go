package github

import (
	"context"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Secondary-rate-limit retry for read-only gh calls.
//
// GitHub's secondary limit refuses a burst even when the primary budget has
// plenty left, so a refusal says "too fast", not "out of quota": waiting a
// little and asking again works, and failing the run on the first refusal does
// not. Only reads go through runRead. A write (pr create/edit, comment, push
// attestation) is never retried here, because replaying it after a refusal
// that may have been applied would duplicate it.
const (
	// rateLimitMaxAttempts bounds the total tries of one call (so at most
	// rateLimitMaxAttempts-1 waits).
	rateLimitMaxAttempts = 4
	// rateLimitBackoffBase is the un-jittered wait before the first retry; it
	// doubles for each later one.
	rateLimitBackoffBase = 5 * time.Second
	// rateLimitMaxWait caps one wait. A Retry-After above it is not retried
	// at all: the refusal is surfaced exactly as before instead of parking
	// the poll for minutes.
	rateLimitMaxWait = 2 * time.Minute
)

// rateLimitMarkers are the lowercase fragments gh prints for a GitHub
// secondary-limit refusal (REST "You have exceeded a secondary rate limit",
// GraphQL "API rate limit already exceeded for user ID", and the legacy
// abuse-detection wording). A bare "HTTP 403" or "HTTP 429" is deliberately
// not enough: a 403 is also a permission failure, which retrying cannot fix.
var rateLimitMarkers = []string{
	"secondary rate limit",
	"rate limit already exceeded",
	"abuse detection",
}

var retryAfterPattern = regexp.MustCompile(`(?i)retry[- ]after:?\s*(\d+)`)

// secondaryRateLimitWait reports whether output is a secondary-rate-limit
// refusal, and how long GitHub asked to wait (zero when it did not say).
// A Retry-After is read only once a marker has matched.
func secondaryRateLimitWait(output string) (wait time.Duration, ok bool) {
	lower := strings.ToLower(output)
	for _, marker := range rateLimitMarkers {
		if !strings.Contains(lower, marker) {
			continue
		}
		if m := retryAfterPattern.FindStringSubmatch(output); m != nil {
			if secs, err := strconv.Atoi(m[1]); err == nil && secs >= 0 {
				return time.Duration(secs) * time.Second, true
			}
		}
		return 0, true
	}
	return 0, false
}

// rateLimitBackoff returns the jittered wait before retry number retry
// (0-based): base*2^retry, capped at rateLimitMaxWait, then drawn uniformly
// from [half, full] so concurrent monitors do not retry in lockstep.
// unit is a draw in [0,1).
func rateLimitBackoff(retry int, unit float64) time.Duration {
	d := rateLimitBackoffBase
	for i := 0; i < retry && d < rateLimitMaxWait; i++ {
		d *= 2
	}
	if d > rateLimitMaxWait {
		d = rateLimitMaxWait
	}
	half := d / 2
	return half + time.Duration(unit*float64(d-half))
}

// rateLimitSleep waits d or until ctx is done.
func rateLimitSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// runRead runs one read-only `gh <args>` and returns its stdout, the failure
// detail (trimmed stderr, or stdout when stderr is empty), and the error. When
// combined is true stderr is merged into the returned output, as
// exec.Cmd.CombinedOutput does. A secondary-rate-limit refusal is retried up
// to rateLimitMaxAttempts times, honouring Retry-After when GitHub sent one
// and backing off exponentially with jitter otherwise.
func (h *Host) runRead(ctx context.Context, combined bool, args ...string) ([]byte, string, error) {
	for attempt := 0; ; attempt++ {
		cmd := h.cmd(ctx, "gh", args...)
		var (
			out    []byte
			detail string
			err    error
		)
		if combined {
			out, err = cmd.CombinedOutput()
			if err != nil {
				detail = strings.TrimSpace(string(out))
			}
		} else {
			out, detail, err = stdoutOnly(cmd)
		}
		if err == nil {
			return out, detail, nil
		}
		wait, limited := secondaryRateLimitWait(detail + "\n" + string(out))
		if !limited || attempt+1 >= rateLimitMaxAttempts || ctx.Err() != nil {
			return out, detail, err
		}
		if wait == 0 {
			//nolint:gosec // non-cryptographic jitter is fine here.
			wait = rateLimitBackoff(attempt, rand.Float64())
		} else if wait > rateLimitMaxWait {
			return out, detail, err
		}
		sleep := h.rateLimitSleep
		if sleep == nil {
			sleep = rateLimitSleep
		}
		if sleepErr := sleep(ctx, wait); sleepErr != nil {
			return out, detail, err
		}
	}
}

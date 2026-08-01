package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type fallbackAgent struct {
	agents []Agent
}

const (
	maxQuotaFallbackAttempts           = 8
	maxQuotaFallbackAgentNameRunes     = 64
	quotaFallbackTruncationMarker      = "..."
	quotaFallbackAttemptsOmittedMarker = "additional attempts omitted"
)

type quotaFallbackAttempt struct {
	agent  string
	reason string
}

type quotaFallbackError struct {
	attempts []quotaFallbackAttempt
	omitted  bool
}

type providerQuotaError struct {
	err    error
	reason string
}

func (e *providerQuotaError) Error() string {
	return fmt.Sprintf("provider quota unavailable: %s", e.reason)
}
func (e *providerQuotaError) Unwrap() error { return e.err }

func (e *quotaFallbackError) Error() string {
	parts := make([]string, 0, len(e.attempts)+1)
	for _, attempt := range e.attempts {
		parts = append(parts, fmt.Sprintf("%s (%s)", attempt.agent, attempt.reason))
	}
	if e.omitted {
		parts = append(parts, quotaFallbackAttemptsOmittedMarker)
	}
	return fmt.Sprintf("configured agent fallback exhausted: %s", strings.Join(parts, "; "))
}

func appendQuotaFallbackAttempt(attempts []quotaFallbackAttempt, omitted bool, agent, reason string) ([]quotaFallbackAttempt, bool) {
	if len(attempts) >= maxQuotaFallbackAttempts {
		return attempts, true
	}
	name := []rune(agent)
	if len(name) > maxQuotaFallbackAgentNameRunes {
		agent = string(name[:maxQuotaFallbackAgentNameRunes]) + quotaFallbackTruncationMarker
	}
	return append(attempts, quotaFallbackAttempt{agent: agent, reason: reason}), omitted
}

// IsQuotaFallbackError reports whether an ordered fallback stopped after an
// explicit quota, session-limit, or rate-limit signal. The error deliberately
// contains only configured agent names and bounded classifications.
func IsQuotaFallbackError(err error) bool {
	var quotaErr *quotaFallbackError
	return errors.As(err, &quotaErr)
}

// NewFallback returns an Agent that tries each agent in order when an
// invocation fails because the current agent process is unavailable or returns
// explicit quota, session-limit, or rate-limit evidence. Quota replacement is
// synchronous and starts the next provider with no provider-native session.
func NewFallback(agents []Agent) Agent {
	if len(agents) == 0 {
		return nil
	}
	copied := make([]Agent, len(agents))
	copy(copied, agents)
	return &fallbackAgent{agents: copied}
}

func (a *fallbackAgent) Name() string {
	if len(a.agents) == 0 {
		return ""
	}
	return a.agents[0].Name()
}

func (a *fallbackAgent) SupportsSessionResume() bool {
	for _, current := range a.agents {
		if SupportsSessionResume(current) {
			return true
		}
	}
	return false
}

func (a *fallbackAgent) SupportsSessionProvider(provider string) bool {
	for _, current := range a.agents {
		if SupportsSessionProvider(current, provider) {
			return true
		}
	}
	return false
}

func (a *fallbackAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions fails closed over the whole fallback set: the
// wrapper may invoke any member, so it neutralizes the target repo's project
// agent-instruction files only if EVERY member does. A single unverified member
// makes the wrapper report false so the gate is refused rather than risk that
// member running unneutralized.
func (a *fallbackAgent) NeutralizesGateInstructions() bool {
	if len(a.agents) == 0 {
		return false
	}
	for _, current := range a.agents {
		if !NeutralizesGateInstructions(current) {
			return false
		}
	}
	return true
}

func (a *fallbackAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	// A resumed session is owned by exactly one provider. Try that provider
	// first, preserving the existing resume-failure contract. A quota signal is
	// the only result that opens the ordered list after that provider; the next
	// provider always starts a fresh session.
	candidateIndexes := make([]int, 0, len(a.agents))
	if opts.Session != nil && opts.Session.ID != "" && opts.Session.Agent != "" {
		providerIndex := -1
		for i, current := range a.agents {
			if SupportsSessionProvider(current, opts.Session.Agent) {
				providerIndex = i
				break
			}
		}
		if providerIndex < 0 {
			return nil, fmt.Errorf("session provider %q is not configured", opts.Session.Agent)
		}
		candidateIndexes = append(candidateIndexes, providerIndex)
	} else {
		for i := range a.agents {
			candidateIndexes = append(candidateIndexes, i)
		}
	}

	scheduled := make(map[int]bool, len(candidateIndexes))
	for _, index := range candidateIndexes {
		scheduled[index] = true
	}
	attempted := make(map[int]bool, len(candidateIndexes))
	path := make([]quotaFallbackAttempt, 0, min(len(a.agents), maxQuotaFallbackAttempts))
	pathOmitted := false
	quotaSeen := false
	freshProvider := false
	var lastErr error
	for position := 0; position < len(candidateIndexes); position++ {
		index := candidateIndexes[position]
		if attempted[index] {
			continue
		}
		attempted[index] = true
		current := a.agents[index]
		currentOpts := opts
		if freshProvider {
			// Never hand a provider-native session id to a different configured
			// agent. An empty ref asks a capable replacement to start cold; an
			// incapable replacement below clears it entirely.
			if opts.Session != nil {
				currentOpts.Session = &SessionRef{}
			}
			currentOpts.SessionFallback = false
			currentOpts.SessionFallbackReason = ""
		}
		if currentOpts.Session != nil && currentOpts.Session.ID == "" && !SupportsSessionResume(current) {
			currentOpts.Session = nil
			currentOpts.SessionFallback = false
		}
		startedAt := time.Now()
		result, err := current.Run(ctx, currentOpts)
		if !ReportsAgentAttempts(current) {
			emitAgentAttempt(currentOpts, current.Name(), result, err, startedAt, time.Now())
		}
		if err == nil {
			if result != nil && result.Provider == "" {
				result.Provider = current.Name()
			}
			return result, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}

		reason, isQuota := quotaErrorReason(err)
		if isQuota {
			quotaSeen = true
			path, pathOmitted = appendQuotaFallbackAttempt(path, pathOmitted, current.Name(), reason)
			freshProvider = true
			// A quota signal from a resumed provider unlocks only the remaining
			// configured order. This is still synchronous and only happens after
			// the current adapter returned.
			for next := index + 1; next < len(a.agents); next++ {
				if !scheduled[next] {
					candidateIndexes = append(candidateIndexes, next)
					scheduled[next] = true
				}
			}
		} else {
			path, pathOmitted = appendQuotaFallbackAttempt(path, pathOmitted, current.Name(), fallbackFailureReason(err))
		}

		if quotaSeen {
			if position == len(candidateIndexes)-1 || (!isAgentUnavailableError(err) && !isQuota) {
				return nil, &quotaFallbackError{attempts: path, omitted: pathOmitted}
			}
		} else if position == len(candidateIndexes)-1 || !isAgentUnavailableError(err) {
			return nil, err
		}

		nextPosition := position + 1
		if nextPosition >= len(candidateIndexes) {
			if quotaSeen {
				return nil, &quotaFallbackError{attempts: path, omitted: pathOmitted}
			}
			return nil, err
		}
		next := a.agents[candidateIndexes[nextPosition]]
		if opts.OnChunk != nil {
			if isQuota {
				opts.OnChunk(fmt.Sprintf("\nagent %s exhausted (%s); falling back to %s\n", current.Name(), reason, next.Name()))
			} else {
				opts.OnChunk(fmt.Sprintf("\nagent %s failed (%s); falling back to %s\n", current.Name(), fallbackReason(err), next.Name()))
			}
		}
	}
	if quotaSeen {
		return nil, &quotaFallbackError{attempts: path, omitted: pathOmitted}
	}
	return nil, lastErr
}

func (a *fallbackAgent) Close() error {
	if len(a.agents) == 1 {
		return a.agents[0].Close()
	}
	var errs []string
	for _, ag := range a.agents {
		if err := ag.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ag.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close fallback agents: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ClassifyProviderError marks an invocation error using only provider or
// process diagnostics captured separately from assistant output.
func ClassifyProviderError(err error, diagnostics ...string) error {
	if err == nil {
		return nil
	}
	for _, diagnostic := range diagnostics {
		if reason, ok := quotaDiagnosticReason(diagnostic); ok {
			return &providerQuotaError{err: err, reason: reason}
		}
	}
	return err
}

func quotaErrorReason(err error) (string, bool) {
	var quotaErr *providerQuotaError
	if !errors.As(err, &quotaErr) {
		return "", false
	}
	return quotaErr.reason, true
}

func quotaDiagnosticReason(diagnostic string) (string, bool) {
	msg := strings.ToLower(diagnostic)
	if strings.Contains(msg, "session limit") ||
		strings.Contains(msg, "session_limit") ||
		strings.Contains(msg, "usage limit") ||
		strings.Contains(msg, "weekly limit") ||
		strings.Contains(msg, "monthly limit") ||
		strings.Contains(msg, "daily limit") ||
		strings.Contains(msg, "quota exceeded") ||
		strings.Contains(msg, "quota exhausted") ||
		strings.Contains(msg, "quota limit") {
		return "session/quota limit", true
	}
	if strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "rate_limit") ||
		strings.Contains(msg, "rate-limited") ||
		strings.Contains(msg, "rate limited") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "http 429") ||
		strings.Contains(msg, "status 429") {
		return "rate limit", true
	}
	return "", false
}

func fallbackFailureReason(err error) string {
	if isAgentUnavailableError(err) {
		return "agent unavailable"
	}
	return "invocation failed"
}

func isAgentUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	unavailable := []string{
		" start:",
		"start server ",
		" server: start server ",
		" exited:",
		" reported exit code ",
	}
	for _, needle := range unavailable {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func fallbackReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	const max = 160
	if len([]rune(text)) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "..."
}

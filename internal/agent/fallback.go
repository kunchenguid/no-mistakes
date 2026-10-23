package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fallbackAgent struct {
	agents       []Agent
	quotaSwitch  bool
	position     atomic.Int64
	freshSession atomic.Bool
}

// FallbackOption customizes a fallback chain.
type FallbackOption func(*fallbackAgent)

// WithQuotaSwitch activates the quota-aware mid-run switch: a recognised
// quota-exhaustion error moves the run to the next agent in the chosen order
// with a fresh session. Only quota routing turns this on; the plain launch
// fallback never consumes a quota failure, preserving today's behavior when
// the feature is off.
func WithQuotaSwitch(start string) FallbackOption {
	return func(f *fallbackAgent) {
		f.quotaSwitch = true
		for i, current := range f.agents {
			if types.CanonicalAgentName(types.AgentName(current.Name())) == types.CanonicalAgentName(types.AgentName(start)) {
				f.position.Store(int64(i))
				f.freshSession.Store(true)
				break
			}
		}
	}
}

// NewFallback returns an Agent that tries each agent in order when an
// invocation fails because the current agent process is unavailable, or -
// with WithQuotaSwitch - when the active agent's subscription quota ran out.
func NewFallback(agents []Agent, opts ...FallbackOption) Agent {
	if len(agents) == 0 {
		return nil
	}
	if len(agents) == 1 && len(opts) == 0 {
		return agents[0]
	}
	copied := make([]Agent, len(agents))
	copy(copied, agents)
	f := &fallbackAgent{agents: copied}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

func (a *fallbackAgent) Name() string {
	if len(a.agents) == 0 {
		return ""
	}
	return a.agents[a.position.Load()].Name()
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
	start := int(a.position.Load())
	candidates := a.agents[start:]
	var sessionAgent Agent
	if opts.Session != nil && opts.Session.ID != "" && opts.Session.Agent != "" {
		for _, current := range a.agents {
			if SupportsSessionProvider(current, opts.Session.Agent) {
				sessionAgent = current
				break
			}
		}
		if sessionAgent == nil {
			return nil, fmt.Errorf("session provider %q is not configured", opts.Session.Agent)
		}
		// Without the quota switch the session provider is the only
		// candidate, exactly as before. With it the remaining chosen order
		// stays eligible: only the session provider resumes the session,
		// and once a switch fires the session identity is dropped.
		if !a.quotaSwitch {
			candidates = []Agent{sessionAgent}
		}
	}
	var lastErr error
	sessionDropped := false
	for i, current := range candidates {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		sessionDropped = a.freshSession.Swap(false) || sessionDropped
		currentOpts := opts
		if currentOpts.Session != nil {
			if currentOpts.Session.ID != "" {
				if sessionDropped || current != sessionAgent {
					// A different provider, or any agent after a
					// quota switch, continues with a fresh session.
					if SupportsSessionResume(current) {
						currentOpts.Session = &SessionRef{}
					} else {
						currentOpts.Session = nil
					}
					currentOpts.SessionFallback = false
				}
			} else if !SupportsSessionResume(current) {
				currentOpts.Session = nil
				currentOpts.SessionFallback = false
			}
		}
		startedAt := time.Now()
		result, err := current.Run(ctx, currentOpts)
		quotaExhausted := a.quotaSwitch && IsQuotaExhaustionError(err)
		if err != nil {
			if result == nil {
				result = &Result{}
			}
			if result.ResumeRetryable == nil || quotaExhausted {
				retryable := currentOpts.Session != nil && currentOpts.Session.ID != "" && !quotaExhausted
				result.ResumeRetryable = &retryable
			}
		}
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
		// A fallback is another attempt inside this invocation's existing
		// bounded context, not a fresh timeout policy. Once that context has
		// expired or been cancelled, the next candidate cannot start
		// meaningfully. Preserve the current adapter's report for the caller's
		// timeout diagnostic, but do not announce or instrument an attempt that
		// will inherit an already-dead context.
		if context.Cause(ctx) != nil {
			return result, err
		}
		if i == len(candidates)-1 {
			return result, err
		}
		if quotaExhausted {
			next := candidates[i+1]
			position := int64(start + i + 1)
			for previous := a.position.Load(); previous < position; previous = a.position.Load() {
				if a.position.CompareAndSwap(previous, position) {
					break
				}
			}
			emitAgentQuotaSwitch(opts, current.Name(), next.Name(), err)
			sessionDropped = true
			continue
		}
		if isAgentUnavailableError(err) {
			next := candidates[i+1]
			emitAgentFallback(opts, current.Name(), next.Name(), err)
			continue
		}
		return result, err
	}
	return nil, lastErr
}

func (a *fallbackAgent) Close() error {
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

func isAgentUnavailableError(err error) bool {
	if err == nil || IsStructuredOutputRejected(err) {
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

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// liveness.go owns the per-invocation silence watchdog. One invocation gets
// one monotonic last-activity clock; every reported activity kind (stdout
// bytes, native process lifecycle, bound adapter-native session advancement)
// folds into that single clock. The watchdog cancels the invocation only
// after the full configured budget elapses with no activity from any source,
// so a healthy turn whose stdout is buffered (pi JSON mode) stays alive on
// session evidence, while a genuinely frozen agent is still terminated with
// its whole process tree after the same budget the fixed deadline used to
// impose. The clock starts at invocation start and every process start
// re-arms it, which is what gives each newly launched agent a fresh full
// budget instead of inheriting an earlier turn's quiet time.

// invocationLiveness is the single monotonic last-activity owner for one
// agent invocation.
type invocationLiveness struct {
	mu   sync.Mutex
	last time.Time
	seen map[agent.ActivityKind]time.Time
}

func newInvocationLiveness() *invocationLiveness {
	return &invocationLiveness{
		last: time.Now(),
		seen: make(map[agent.ActivityKind]time.Time),
	}
}

func (l *invocationLiveness) record(kind agent.ActivityKind) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.last = now
	l.seen[kind] = now
}

// evidence renders the per-kind activity summary for the timeout diagnostic.
// It names evidence classes and ages only - never content, paths, or session
// payloads - so operators can distinguish stdout-active, session-active, and
// genuinely quiet invocations.
func (l *invocationLiveness) evidence() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) == 0 {
		return "no stdout, lifecycle, or session-event activity observed this invocation"
	}
	type kindAge struct {
		label string
		age   time.Duration
		at    time.Time
	}
	now := time.Now()
	parts := make([]kindAge, 0, len(l.seen))
	for kind, at := range l.seen {
		parts = append(parts, kindAge{label: agent.ActivityKindLabel(kind), age: now.Sub(at).Round(time.Millisecond), at: at})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].at.After(parts[j].at) })
	rendered := make([]string, 0, len(parts))
	for _, part := range parts {
		rendered = append(rendered, fmt.Sprintf("%s %s ago", part.label, part.age))
	}
	return "last activity: " + strings.Join(rendered, ", ")
}

// AgentTimeoutError is the watchdog's cancellation cause and diagnostic. It
// carries the applied budget plus the liveness evidence at kill time so step
// error mapping can re-render the same evidence under a step-specific label
// without re-deriving it. It unwraps to ErrAgentTimeout so existing
// errors.Is callers keep working.
type AgentTimeoutError struct {
	Budget   time.Duration
	Evidence string
}

func (e *AgentTimeoutError) Error() string {
	return fmt.Sprintf("agent timed out after %s (agent silent for %s%s): %s",
		e.Budget, e.Budget, evidenceClause(e.Evidence), ErrAgentTimeout)
}

func (e *AgentTimeoutError) Unwrap() error { return ErrAgentTimeout }

// StepError re-renders the timeout under a step-owned label, preserving the
// budget and evidence: "agent review timed out after 30m0s (review agent
// silent for 30m0s: <evidence>): agent timeout".
func (e *AgentTimeoutError) StepError(prefix, silentLabel string) error {
	return fmt.Errorf("%s timed out after %s (%s silent for %s%s): %w",
		prefix, e.Budget, silentLabel, e.Budget, evidenceClause(e.Evidence), ErrAgentTimeout)
}

func evidenceClause(evidence string) string {
	if evidence == "" {
		return ""
	}
	return ": " + evidence
}

// watchSilence derives a cancellation context from parent and starts the
// watchdog goroutine for l. When l stays silent for the whole budget, the
// watchdog cancels the context with an *AgentTimeoutError cause. The
// goroutine is owned by the returned context: it exits when the watchdog
// fires or when the caller cancels, whichever comes first.
func watchSilence(parent context.Context, budget time.Duration, l *invocationLiveness) (context.Context, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(parent)
	cancel := func() { cancelCause(nil) }
	go func() {
		ticker := time.NewTicker(silenceWatchInterval(budget))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			l.mu.Lock()
			silentFor := time.Since(l.last)
			l.mu.Unlock()
			if silentFor >= budget {
				cancelCause(&AgentTimeoutError{Budget: budget, Evidence: l.evidence()})
				return
			}
		}
	}()
	return ctx, cancel
}

// silenceWatchInterval scales the watchdog tick to the budget so short test
// budgets still fire promptly while production budgets poll cheaply.
func silenceWatchInterval(budget time.Duration) time.Duration {
	interval := budget / 10
	if interval < 2*time.Millisecond {
		return 2 * time.Millisecond
	}
	if interval > 10*time.Second {
		return 10 * time.Second
	}
	return interval
}

// asAgentTimeout extracts the watchdog's timeout diagnostic from an error
// chain, returning nil when the failure was something else.
func asAgentTimeout(err error) *AgentTimeoutError {
	return AsAgentTimeout(err)
}

// AsAgentTimeout extracts the silence watchdog's timeout diagnostic from an
// error chain, returning nil when the failure was something else. Steps with
// a step-labeled timeout message use it to re-render the budget and evidence
// under their own label.
func AsAgentTimeout(err error) *AgentTimeoutError {
	var ate *AgentTimeoutError
	if errors.As(err, &ate) {
		return ate
	}
	return nil
}

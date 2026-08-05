package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DefaultMaxConcurrentInvocations is the daemon-wide safety ceiling for active
// provider/model calls. Operators may lower it in global config, never raise it.
const DefaultMaxConcurrentInvocations = 2

// InvocationLimiter admits provider/model calls in FIFO order. It belongs to a
// daemon instance: cancellation removes a queued call, and process shutdown
// drops the instance with no state to recover or persist.
type InvocationLimiter struct {
	mu      sync.Mutex
	limit   int
	active  int
	waiters []*invocationWaiter
}

type invocationWaiter struct {
	ready   chan struct{}
	granted bool
}

// NewInvocationLimiter creates a limiter with a positive limit at or below the
// daemon-wide safety ceiling. Invalid values fail closed to the ceiling; config
// validation is responsible for reporting operator mistakes before this point.
func NewInvocationLimiter(limit int) *InvocationLimiter {
	if limit < 1 || limit > DefaultMaxConcurrentInvocations {
		limit = DefaultMaxConcurrentInvocations
	}
	return &InvocationLimiter{limit: limit}
}

// WithInvocationLimiter propagates limiter ownership through decorators and
// fallback agents. Native adapters acquire a slot around each concrete attempt,
// rather than around a whole retry/fallback sequence.
func WithInvocationLimiter(inner Agent, limiter *InvocationLimiter) Agent {
	if inner == nil || limiter == nil {
		return inner
	}
	return &limitedAgent{inner: inner, limiter: limiter}
}

type limitedAgent struct {
	inner   Agent
	limiter *InvocationLimiter
}

func (a *limitedAgent) Name() string { return a.inner.Name() }
func (a *limitedAgent) Close() error { return a.inner.Close() }
func (a *limitedAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	opts.invocationLimiter = a.limiter
	return a.inner.Run(ctx, opts)
}
func (a *limitedAgent) SupportsSessionResume() bool { return SupportsSessionResume(a.inner) }
func (a *limitedAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.inner, provider)
}
func (a *limitedAgent) ReportsAgentAttempts() bool { return ReportsAgentAttempts(a.inner) }
func (a *limitedAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.inner)
}

// acquire waits fairly for one slot. The returned release is idempotent so
// deferred cleanup remains safe when an adapter returns an error or panics.
func (l *InvocationLimiter) acquire(ctx context.Context, name string, onLifecycle func(LifecycleEvent)) (func(), error) {
	if err := ctx.Err(); err != nil {
		return func() {}, err
	}
	if l == nil {
		return func() {}, nil
	}
	started := time.Now()
	waiter := &invocationWaiter{ready: make(chan struct{})}

	l.mu.Lock()
	if l.active < l.limit && len(l.waiters) == 0 {
		l.active++
		active := l.active
		l.mu.Unlock()
		l.emitAdmitted(name, 0, active, onLifecycle)
		return l.releaseFunc(), nil
	}
	l.waiters = append(l.waiters, waiter)
	position, active := len(l.waiters), l.active
	l.mu.Unlock()
	if onLifecycle != nil {
		onLifecycle(LifecycleEvent{
			Agent:   name,
			Phase:   LifecyclePhaseQueue,
			Message: fmt.Sprintf("%s waiting for global model slot (queue_position=%d active_slots=%d/%d)", name, position, active, l.limit),
		})
	}

	select {
	case <-waiter.ready:
		// If cancellation won just as a slot was assigned, return it before
		// starting the provider. This keeps a cancelled queued call from
		// becoming an invocation merely because select chose ready.
		if err := ctx.Err(); err != nil {
			l.releaseFunc()()
			return func() {}, err
		}
		l.mu.Lock()
		active = l.active
		l.mu.Unlock()
		l.emitAdmitted(name, time.Since(started), active, onLifecycle)
		return l.releaseFunc(), nil
	case <-ctx.Done():
		l.mu.Lock()
		if waiter.granted {
			// A release won the race with cancellation. Return the admission to
			// the next FIFO waiter before reporting cancellation to the caller.
			l.releaseLocked()
			l.mu.Unlock()
			return func() {}, ctx.Err()
		}
		for i, candidate := range l.waiters {
			if candidate == waiter {
				l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
				break
			}
		}
		l.mu.Unlock()
		return func() {}, ctx.Err()
	}
}

func (l *InvocationLimiter) emitAdmitted(name string, wait time.Duration, active int, onLifecycle func(LifecycleEvent)) {
	if onLifecycle == nil {
		return
	}
	onLifecycle(LifecycleEvent{
		Agent:   name,
		Phase:   LifecyclePhaseAdmitted,
		Message: fmt.Sprintf("%s admitted to global model slot (wait_ms=%d active_slots=%d/%d)", name, wait.Milliseconds(), active, l.limit),
	})
}

func (l *InvocationLimiter) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.releaseLocked()
			l.mu.Unlock()
		})
	}
}

func (l *InvocationLimiter) releaseLocked() {
	if l.active <= 0 {
		return
	}
	l.active--
	if len(l.waiters) == 0 {
		return
	}
	waiter := l.waiters[0]
	l.waiters = l.waiters[1:]
	waiter.granted = true
	l.active++
	close(waiter.ready)
}

func (l *InvocationLimiter) snapshot() (active, waiting, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, len(l.waiters), l.limit
}

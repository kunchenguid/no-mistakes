package agent

import (
	"fmt"
	"sync"
	"time"
)

const (
	// LifecyclePhaseStart marks native subprocess startup.
	LifecyclePhaseStart = "start"
	// LifecyclePhaseExit marks native subprocess exit.
	LifecyclePhaseExit = "exit"
	// LifecyclePhaseRetry marks a transient retry before the next subprocess attempt.
	LifecyclePhaseRetry = "retry"
	// LifecyclePhaseFallback marks any fallback before a fresh agent attempt,
	// including provider, session-resume, and structured-output fallbacks.
	LifecyclePhaseFallback = "fallback"
	// LifecyclePhaseActivity marks observed liveness of a running native
	// subprocess: bytes arrived on its stdout or stderr.
	//
	// Start and exit alone cannot tell a wedged agent from a working one. Every
	// adapter forwards only assistant prose to OnChunk, and an agent spends most
	// of a long turn emitting tool events instead, so a healthy multi-minute fix
	// round is indistinguishable from a process that is blocked before its first
	// byte. Together with streamed assistant text, this phase supplies the
	// measured output evidence used by invocation-timeout diagnostics.
	LifecyclePhaseActivity = "activity"
	// LifecyclePhaseProgress marks observed forward motion in a running agent
	// turn: assistant prose, a tool call, or a tool result.
	//
	// Activity and progress answer different questions. Activity is satisfied by
	// any byte on stdout or stderr, and an agent CLI emits those continuously
	// while it thinks - so a turn that never converges keeps every byte-level
	// liveness signal green while producing nothing an operator can act on.
	// Progress is the signal an inactivity bound can trust: it resets only when
	// the turn actually advances. See DefaultAgentStallTimeout for what that
	// bound exists to catch.
	//
	// Only adapters that parse a structured event stream report this phase.
	// Adapters that forward assistant prose alone contribute it through
	// OnChunk, which is the same forward motion.
	LifecyclePhaseProgress = "progress"
)

// nativeAgentActivityInterval throttles LifecyclePhaseActivity so a chatty
// subprocess cannot flood the observer. It is a liveness signal, not a log: the
// first byte of a quiet period is reported immediately and further bytes are
// coalesced until the interval elapses.
const nativeAgentActivityInterval = 5 * time.Second

func emitAgentStarted(opts RunOpts, name string, pid int) {
	emitLifecycle(opts, LifecycleEvent{
		Agent:   name,
		Phase:   LifecyclePhaseStart,
		PID:     pid,
		Message: fmt.Sprintf("%s started pid=%d", name, pid),
	})
}

func emitAgentExited(opts RunOpts, name string, pid int, err error) {
	message := fmt.Sprintf("%s exited pid=%d status=success", name, pid)
	if err != nil {
		message = fmt.Sprintf("%s exited pid=%d error=%s", name, pid, err.Error())
	}
	emitLifecycle(opts, LifecycleEvent{
		Agent:   name,
		Phase:   LifecyclePhaseExit,
		PID:     pid,
		Message: message,
	})
}

// nativeAgentActivityObserver returns the throttled liveness callback handed to
// startNativeAgentCommand, or nil when nobody is observing this invocation.
// Returning nil keeps the read path allocation-free for callers that do not
// care (tests, eval replay).
func nativeAgentActivityObserver(opts RunOpts, name string) func() {
	if opts.OnLifecycle == nil {
		return nil
	}
	var (
		mu       sync.Mutex
		lastEmit time.Time
	)
	return func() {
		now := time.Now()
		mu.Lock()
		if !lastEmit.IsZero() && now.Sub(lastEmit) < nativeAgentActivityInterval {
			mu.Unlock()
			return
		}
		lastEmit = now
		mu.Unlock()
		emitLifecycle(opts, LifecycleEvent{
			Agent:   name,
			Phase:   LifecyclePhaseActivity,
			Message: fmt.Sprintf("%s producing output", name),
		})
	}
}

func emitAgentRetry(opts RunOpts, name string, label string, attempt, max int) {
	message := fmt.Sprintf("%s retrying after transient error %q (attempt %d/%d)", name, label, attempt, max)
	emitAgentControl(opts, LifecycleEvent{
		Agent:   name,
		Phase:   LifecyclePhaseRetry,
		Message: message,
	})
}

func emitAgentFallback(opts RunOpts, current, next string, err error) {
	emitAgentControl(opts, LifecycleEvent{
		Agent:   current,
		Phase:   LifecyclePhaseFallback,
		Message: fmt.Sprintf("agent %s failed (%s); falling back to %s", current, fallbackReason(err), next),
	})
}

func emitAgentControl(opts RunOpts, event LifecycleEvent) {
	if opts.OnLifecycle != nil {
		emitLifecycle(opts, event)
		return
	}
	if opts.OnChunk != nil {
		opts.OnChunk(event.Message)
	}
}

// agentProgressEmitter returns the callback a structured-stream parser uses to
// report a turn advance, or nil when nobody is observing this invocation.
//
// Returning nil keeps the parse path allocation-free and branch-free for
// callers that do not care (eval replay), which is the same contract
// nativeAgentActivityObserver follows for byte-level liveness.
//
// Progress travels as a lifecycle event rather than through OnChunk because a
// tool call has no text to stream: routing it through OnChunk would write tool
// bookkeeping into the step log, which is exactly what the log exists not to
// contain. When no lifecycle observer is installed there is no sink for
// progress at all, so the emitter stays nil rather than inventing one.
func agentProgressEmitter(opts RunOpts, name string) func() {
	if opts.OnLifecycle == nil {
		return nil
	}
	return func() {
		emitLifecycle(opts, LifecycleEvent{
			Agent: name,
			Phase: LifecyclePhaseProgress,
		})
	}
}

func emitLifecycle(opts RunOpts, event LifecycleEvent) {
	if opts.OnLifecycle != nil {
		opts.OnLifecycle(event)
	}
}

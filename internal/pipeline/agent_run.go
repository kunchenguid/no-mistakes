package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// ErrAgentTimeout is the context cause used when the default per-invocation
// agent deadline expires. Callers wrap it with a diagnostic that names the
// budget; a late successful return after this cause is still a timeout.
var ErrAgentTimeout = errors.New("agent timeout")

// AgentTimeout is the per-invocation budget applied at the shared agent-run
// seam. A positive Config.AgentTimeout wins; otherwise the default (30m).
func AgentTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.AgentTimeout > 0 {
		return cfg.AgentTimeout
	}
	return config.DefaultAgentTimeout
}

// RunAgent executes one agent invocation with a deadline scoped only to that
// call. The parent StepContext.Ctx is left unchanged so post-agent work
// (commits, git, parsing) is not cancelled by the invocation budget.
//
// If the parent context already has a deadline (review's round budget, Test's
// explicit wrap, intent extraction, caller cancellation), that bound is
// honored and no shorter default is stacked. Otherwise AgentTimeout is
// applied. A late successful return after the deadline is rejected.
func (sctx *StepContext) RunAgent(opts agent.RunOpts) (*agent.Result, error) {
	parent := context.Background()
	if sctx != nil {
		parent = sctx.Ctx
	}
	return sctx.runAgent(parent, opts, "")
}

// RunAgentContext is RunAgent with an explicit parent, used when a step has
// already installed a more specific deadline (review round, Test invocation).
func (sctx *StepContext) RunAgentContext(parent context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, "")
}

// RunAgentSessionContext is RunAgentSession with an explicit parent so a
// fixer turn can share a round budget (review) or a per-invocation wrap (Test).
func (sctx *StepContext) RunAgentSessionContext(parent context.Context, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, role)
}

func (sctx *StepContext) runAgent(parent context.Context, opts agent.RunOpts, sessionRole SessionRole) (*agent.Result, error) {
	var ag agent.Agent
	timeout := AgentTimeout(nil)
	if sctx != nil {
		ag = sctx.Agent
		timeout = AgentTimeout(sctx.Config)
	}
	activity := observeAgentActivity(&opts)
	return invokeAgent(parent, timeout, activity, func(ctx context.Context) (*agent.Result, error) {
		if sessionRole != "" && sctx != nil && sctx.Sessions != nil {
			return sctx.Sessions.Run(ctx, ag, sessionRole, opts, sctx.Log)
		}
		if ag == nil {
			return nil, errors.New("nil agent")
		}
		return ag.Run(ctx, opts)
	})
}

func invokeAgent(parent context.Context, timeout time.Duration, activity *agentActivity, run func(context.Context) (*agent.Result, error)) (*agent.Result, error) {
	ctx, cancel, applied := bindAgentDeadline(parent, timeout)
	result, err := run(ctx)
	runErr := classifyAgentRun(ctx, applied, activity, err)
	cancel()
	if runErr != nil {
		return nil, runErr
	}
	return result, nil
}

// agentActivity records when an in-flight invocation last produced anything
// observable: streamed assistant text or raw subprocess bytes
// (agent.LifecyclePhaseActivity). Lifecycle control metadata is not output.
//
// It exists because the timeout diagnostics used to assert that the agent had
// been "silent for <budget>" without ever measuring silence - the budget was
// simply printed twice. An operator reading that line cannot tell a wedged
// process from one that streamed until the last second, which is exactly the
// distinction that decides whether to re-run, raise the budget, or go look at
// the agent CLI. Everything reported now is measured.
type agentActivity struct {
	mu sync.Mutex
	// begun is when the current attempt was handed to the agent.
	begun time.Time
	// last is when output was most recently observed; zero when none ever was.
	last time.Time
	// observed counts output events. A subprocess launch is deliberately not
	// one of them: launching proves the binary ran, not that it is doing
	// anything, and counting it would erase the difference this whole
	// measurement exists to expose.
	observed int
	// launchedPID is the native subprocess PID, when one was reported.
	launchedPID int
	launchedAt  time.Time
	launched    bool
}

func newAgentActivity() *agentActivity {
	return &agentActivity{begun: time.Now()}
}

func (a *agentActivity) observe() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.observed++
	a.last = time.Now()
	a.mu.Unlock()
}

func (a *agentActivity) beginAttempt() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.begun = time.Now()
	a.last = time.Time{}
	a.observed = 0
	a.launchedPID = 0
	a.launchedAt = time.Time{}
	a.launched = false
	a.mu.Unlock()
}

func (a *agentActivity) observeLaunch(pid int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.launched = true
	a.launchedPID = pid
	a.launchedAt = time.Now()
	a.mu.Unlock()
}

// evidence renders what was actually observed, for the timeout message.
func (a *agentActivity) evidence() string {
	if a == nil {
		return "agent activity was not observed for this invocation"
	}
	a.mu.Lock()
	observed, begun, last := a.observed, a.begun, a.last
	launched, launchedAt, pid := a.launched, a.launchedAt, a.launchedPID
	a.mu.Unlock()
	if observed > 0 {
		return fmt.Sprintf("agent last produced output %s ago (%d observed)",
			roundActivity(time.Since(last)), observed)
	}
	if launched {
		return fmt.Sprintf("agent produced no output at all in %s after its subprocess started (pid=%d)",
			roundActivity(time.Since(launchedAt)), pid)
	}
	return fmt.Sprintf("agent produced no output at all in %s and never reported a subprocess start",
		roundActivity(time.Since(begun)))
}

func roundActivity(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}

// observeAgentActivity instruments opts so every streamed chunk and every
// native lifecycle event is recorded before it reaches the caller's callbacks.
// The wrappers are pure observers: they always forward.
func observeAgentActivity(opts *agent.RunOpts) *agentActivity {
	activity := newAgentActivity()
	onChunk := opts.OnChunk
	opts.OnChunk = func(text string) {
		activity.observe()
		if onChunk != nil {
			onChunk(text)
		}
	}
	onLifecycle := opts.OnLifecycle
	opts.OnLifecycle = func(event agent.LifecycleEvent) {
		switch event.Phase {
		case agent.LifecyclePhaseStart:
			// Launching proves the binary ran, not that it is doing anything.
			activity.observeLaunch(event.PID)
		case agent.LifecyclePhaseActivity:
			activity.observe()
		case agent.LifecyclePhaseRetry, agent.LifecyclePhaseFallback:
			activity.beginAttempt()
		case agent.LifecyclePhaseExit:
			// Exit is the deadline's own consequence: cancelling the context
			// kills the subprocess and the adapter reports it. Counting that as
			// agent output would make every timeout claim the agent was busy
			// until the last instant, which is the fabricated-evidence problem
			// this measurement replaces.
		default:
			// Unknown lifecycle phases are adapter control metadata, not evidence
			// of assistant text or subprocess output.
		}
		if onLifecycle != nil {
			onLifecycle(event)
		}
	}
	return activity
}

func bindAgentDeadline(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return parent, func() {}, 0
	}
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}, 0
	}
	ctx, cancel := context.WithTimeoutCause(parent, timeout, ErrAgentTimeout)
	return ctx, cancel, timeout
}

func classifyAgentRun(ctx context.Context, applied time.Duration, activity *agentActivity, err error) error {
	cause := context.Cause(ctx)
	if cause == nil {
		return err
	}
	// Only a deadline earns a diagnosis. A plain cancellation (operator abort,
	// daemon shutdown) is already self-explanatory and must not be dressed up
	// as an agent fault.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if applied > 0 && errors.Is(cause, ErrAgentTimeout) {
			return diagnoseAgentTimeout(
				fmt.Sprintf("agent timed out after %s", applied), activity, err, cause)
		}
		// The budget belongs to the caller (a review round, a Test invocation).
		// Keep its cause identity so the caller's own classifier still matches,
		// and hand it the measurement plus whatever the adapter managed to say.
		return diagnoseAgentTimeout("", activity, err, cause)
	}
	return cause
}

// diagnoseAgentTimeout builds the one error a timed-out invocation returns. It
// always carries the measured activity evidence and, crucially, whatever the
// adapter reported - a killed native agent's stderr and exit status is the only
// account of what the process was doing, and dropping it is what made this
// failure mode undiagnosable in the first place.
func diagnoseAgentTimeout(prefix string, activity *agentActivity, adapterErr, cause error) error {
	parts := make([]string, 0, 3)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	parts = append(parts, activity.evidence())
	if clause := agentReportClause(adapterErr); clause != "" {
		parts = append(parts, clause)
	}
	return &agentInvocationError{
		message: strings.Join(parts, "; "),
		cause:   cause,
		adapter: adapterErr,
	}
}

// agentReportClause renders the adapter's own error for the timeout message.
// A nil error, or one that only restates the cancellation the deadline caused,
// adds nothing and is dropped.
func agentReportClause(err error) string {
	if err == nil {
		return ""
	}
	// A bare context error is the deadline we are already reporting, echoed back
	// by the adapter. It adds no account of what the process was doing.
	if err.Error() == context.DeadlineExceeded.Error() || err.Error() == context.Canceled.Error() {
		return ""
	}
	text := safeurl.RedactText(strings.Join(strings.Fields(err.Error()), " "))
	if text == "" {
		return ""
	}
	const max = 400
	if len([]rune(text)) > max {
		text = string([]rune(text)[:max]) + "..."
	}
	return "agent reported: " + text
}

// agentInvocationError carries both the deadline cause (so a step's own
// sentinel keeps matching) and the adapter's error (so the concrete failure
// stays matchable, not just quoted in the message).
type agentInvocationError struct {
	message string
	cause   error
	adapter error
}

func (e *agentInvocationError) Error() string { return e.message }

func (e *agentInvocationError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if e.adapter != nil {
		errs = append(errs, e.adapter)
	}
	return errs
}

// timeoutAgent is the executor backstop: every sctx.Agent.Run is bounded even
// if a future step forgets RunAgent. Nested with RunAgent it is a no-op when
// the incoming context already has a deadline.
type timeoutAgent struct {
	inner   agent.Agent
	timeout time.Duration
}

func (a *timeoutAgent) Name() string { return a.inner.Name() }

func (a *timeoutAgent) Close() error { return a.inner.Close() }

func (a *timeoutAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if _, bounded := ctx.Deadline(); bounded {
		// An outer seam (RunAgent, a review round, a Test wrap) already owns the
		// budget and the diagnosis for this invocation; re-diagnosing here would
		// nest the same measurement inside itself. The backstop this wrapper
		// exists for still holds: a result produced after the deadline is
		// refused, so work from an expired turn can never reach a commit.
		result, err := a.inner.Run(ctx, opts)
		cause := context.Cause(ctx)
		switch {
		case cause == nil:
			return result, err
		case err != nil:
			// The adapter's own account beats restating the cause.
			return nil, err
		default:
			return nil, cause
		}
	}
	activity := observeAgentActivity(&opts)
	return invokeAgent(ctx, a.timeout, activity, func(runCtx context.Context) (*agent.Result, error) {
		return a.inner.Run(runCtx, opts)
	})
}

func (a *timeoutAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *timeoutAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *timeoutAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *timeoutAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

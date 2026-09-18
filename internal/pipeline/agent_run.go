package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// ErrAgentTimeout is the context cause used when the default per-invocation
// agent deadline expires. Callers wrap it with a diagnostic that names the
// budget; a late successful return after this cause is still a timeout.
var ErrAgentTimeout = errors.New("agent timeout")

// ErrAgentStall is the context cause used when an invocation stops advancing
// its turn (see LifecyclePhaseProgress) for longer than its progress bound.
//
// It is distinct from ErrAgentTimeout on purpose. A wall-clock expiry means the
// turn was still working and ran out of budget, which is a sizing decision; a
// stall expiry means the turn stopped converging at all, which is a fault. Both
// fail the invocation, and only the diagnostic and the operator's response
// differ - so callers that already treat an agent failure as fatal need no
// change, while a caller that wants to distinguish them can.
var ErrAgentStall = errors.New("agent stalled")

// AgentTimeout is the per-invocation budget applied at the shared agent-run
// seam. A positive Config.AgentTimeout wins; otherwise the default (30m).
func AgentTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.AgentTimeout > 0 {
		return cfg.AgentTimeout
	}
	return config.DefaultAgentTimeout
}

// AgentStallTimeout is the per-invocation progress bound applied at the shared
// agent-run seam. A positive Config.AgentStallTimeout wins; otherwise the
// default (30m). config.AgentStallUnlimited disables the bound, leaving the
// absolute wall-clock limits as the only ceiling.
func AgentStallTimeout(cfg *config.Config) time.Duration {
	if cfg == nil {
		return config.DefaultAgentStallTimeout
	}
	switch {
	case cfg.AgentStallTimeout > 0:
		return cfg.AgentStallTimeout
	case cfg.AgentStallTimeout == config.AgentStallUnlimited:
		return 0
	default:
		// Zero means the field was never populated (a hand-built Config in a
		// test, say), so the safe default applies rather than silently
		// removing the bound.
		return config.DefaultAgentStallTimeout
	}
}

// RunAgent executes one agent invocation with a deadline scoped only to that
// call. The parent StepContext.Ctx is left unchanged so post-agent work
// (commits, git, parsing) is not cancelled by the invocation budget.
//
// If the parent context already has a deadline (a Review or Test invocation's
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
// already installed a more specific per-invocation deadline (Review or Test).
func (sctx *StepContext) RunAgentContext(parent context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, "")
}

// RunAgentSessionContext is RunAgentSession with an explicit parent so a
// fixer turn can use a step-specific per-invocation wall-clock limit.
func (sctx *StepContext) RunAgentSessionContext(parent context.Context, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, role)
}

func (sctx *StepContext) runAgent(parent context.Context, opts agent.RunOpts, sessionRole SessionRole) (*agent.Result, error) {
	var ag agent.Agent
	timeout := AgentTimeout(nil)
	stall := AgentStallTimeout(nil)
	if sctx != nil {
		ag = sctx.Agent
		timeout = AgentTimeout(sctx.Config)
		stall = AgentStallTimeout(sctx.Config)
	}
	activity := observeAgentActivity(&opts)
	return invokeAgent(parent, timeout, stall, activity, func(ctx context.Context) (*agent.Result, error) {
		if sessionRole != "" && sctx != nil && sctx.Sessions != nil {
			return sctx.Sessions.Run(ctx, ag, sessionRole, opts, sctx.Log)
		}
		if ag == nil {
			return nil, errors.New("nil agent")
		}
		return ag.Run(ctx, opts)
	})
}

// invokeAgent runs one agent invocation under both of its inactivity bounds.
//
// timeout is the absolute wall-clock ceiling and stall is the progress bound
// (see ErrAgentStall): the first bounds the whole turn, the second bounds a turn
// that is still emitting bytes but has stopped advancing. Both are independent
// - a stall expiry ends the invocation early rather than extending the wall
// clock, so no configuration makes a wedged turn outlive its budget.
func invokeAgent(parent context.Context, timeout, stall time.Duration, activity *agentActivity, run func(context.Context) (*agent.Result, error)) (*agent.Result, error) {
	ctx, cancelDeadline, applied := bindAgentDeadline(parent, timeout)
	// The stall bound cancels the invocation itself, so it needs a cancel
	// function it owns. bindAgentDeadline deliberately hands back a no-op cancel
	// when the caller already installed a deadline - and Review and Test both do
	// - which is precisely the case where a stalled turn is most expensive. An
	// independent cancellable child keeps the stall effective there while
	// leaving the caller's context (and its deadline cause) untouched, so the
	// wall-clock diagnosis below still sees the cause it expects.
	runCtx, cancelRun := context.WithCancel(ctx)
	stopWatch := watchAgentStall(runCtx, cancelRun, stall, activity)
	// Cleanup is deferred so a panicking adapter cannot strand the watcher
	// goroutine or leave the layered context live. stopWatch is once-guarded, so
	// the explicit call below and this deferred one cannot race.
	defer cancelRun()
	defer cancelDeadline()
	defer func() { stopWatch() }()
	result, err := run(runCtx)
	stalled := stopWatch()
	runErr := classifyAgentRun(ctx, applied, activity, stalled, err)
	if runErr != nil {
		return nil, runErr
	}
	return result, nil
}

// watchAgentStall cancels ctx when an in-flight invocation stops advancing its
// turn for longer than stall. It returns a function that stops watching and
// reports whether the bound is what ended the invocation.
//
// The verdict is carried out of band - not as a context cause - because the
// deadline this seam installs already owns the context's cause, and
// context.WithTimeoutCause hands back a plain CancelFunc with no way to
// override it. The watcher is the only thing that knows a stall fired, so it
// reports it directly. A non-positive stall disables the bound.
//
// The check is polled rather than scheduled per event because the signal it
// watches is the ABSENCE of events: an event-driven timer would have to be
// re-armed from the agent's own callbacks, which run on the path that is by
// definition silent when this matters. The interval is a fraction of the bound
// so detection is prompt without busy-waiting, and it is floored so a
// deliberately tiny bound (tests, pathological configs) cannot spin.
func watchAgentStall(ctx context.Context, cancel context.CancelFunc, stall time.Duration, activity *agentActivity) func() bool {
	if stall <= 0 || activity == nil {
		return func() bool { return false }
	}
	interval := stall / 4
	if interval < 25*time.Millisecond {
		interval = 25 * time.Millisecond
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	var fired atomic.Bool
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				silent, _, ok := activity.progressSilence()
				if ok && silent >= stall {
					fired.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() bool {
		once.Do(func() {
			close(done)
			<-stopped
		})
		return fired.Load()
	}
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
	// progressed is when this attempt last advanced its turn, and progressCount
	// counts those advances. Progress is strictly narrower than output: it
	// requires assistant text or a tool call/result, so thinking-only byte
	// traffic keeps output fresh while leaving progress stale. That gap is what
	// the stall bound watches.
	progressed    time.Time
	progressCount int
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
	a.progressed = time.Time{}
	a.progressCount = 0
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

// observeProgress records forward motion in the turn. Unlike observe it is not
// satisfied by arbitrary bytes, so it is the signal an inactivity bound can
// trust.
func (a *agentActivity) observeProgress() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.progressCount++
	a.progressed = time.Now()
	a.mu.Unlock()
}

// progressSilence reports how long this attempt has gone without advancing its
// turn, measured from the start of the attempt when nothing has advanced yet.
// ok is false when there is nothing to measure (no observer, or a stall bound
// that is switched off).
func (a *agentActivity) progressSilence() (time.Duration, int, bool) {
	if a == nil {
		return 0, 0, false
	}
	a.mu.Lock()
	begun, progressed, count := a.begun, a.progressed, a.progressCount
	a.mu.Unlock()
	if progressed.IsZero() {
		return time.Since(begun), 0, true
	}
	return time.Since(progressed), count, true
}

// progressEvidence renders the measured stall for the stall-bound diagnostic.
// It names the longest single silent stretch, which is what decides whether to
// raise the bound or go look at the agent CLI.
func (a *agentActivity) progressEvidence() string {
	silent, count, ok := a.progressSilence()
	if !ok {
		return "agent progress was not observed for this invocation"
	}
	if count == 0 {
		return fmt.Sprintf("agent produced no assistant output or tool activity at all in %s",
			roundActivity(silent))
	}
	return fmt.Sprintf("agent produced no assistant output or tool activity for %s (%d progress events observed earlier in the turn)",
		roundActivity(silent), count)
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
		// Prose is forward motion, and for an adapter that only streams text
		// this is the sole progress signal available. Adapters that parse a
		// structured event stream report finer-grained progress through
		// LifecyclePhaseProgress below.
		//
		// emitAgentControl routes retry/fallback messages here when no
		// OnLifecycle is installed, which would count a control message as a
		// turn advance. That can only delay stall detection by one event, never
		// mask a stall, and the pipeline always installs OnLifecycle (so control
		// messages take the retry/fallback branch instead and restart the
		// attempt's clock).
		if text != "" {
			activity.observeProgress()
		}
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
		case agent.LifecyclePhaseProgress:
			activity.observe()
			activity.observeProgress()
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

func classifyAgentRun(ctx context.Context, applied time.Duration, activity *agentActivity, stalled bool, err error) error {
	// A stall expiry is the watchAgentStall verdict, not a context cause: a
	// stall cancels the invocation rather than letting its deadline pass, so
	// ctx.Err() is Canceled and the cause is unavailable here. It is checked
	// first because it is the more specific account of why the turn ended - a
	// turn that stalled would otherwise be reported as a plain cancellation.
	if stalled {
		return diagnoseAgentStall(activity, err)
	}
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
		// The budget belongs to the caller (a Review or Test invocation).
		// Keep its cause identity so the caller's own classifier still matches,
		// and hand it the measurement plus whatever the adapter managed to say.
		return diagnoseAgentTimeout("", activity, err, cause)
	}
	return cause
}

// diagnoseAgentStall builds the failure a stalled invocation returns. It reads
// the same measured evidence as a timeout but reports the opposite finding: not
// that the turn ran out of budget while working, but that it stopped advancing
// altogether. The adapter's own account is still appended, because a killed
// subprocess's exit status and stderr remain the only view inside the agent.
func diagnoseAgentStall(activity *agentActivity, adapterErr error) error {
	parts := []string{"agent made no progress; " + activity.progressEvidence()}
	if clause := agentReportClause(adapterErr); clause != "" {
		parts = append(parts, clause)
	}
	return &agentInvocationError{
		message: strings.Join(parts, "; "),
		cause:   ErrAgentStall,
		adapter: adapterErr,
	}
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
	stall   time.Duration
}

func (a *timeoutAgent) Name() string { return a.inner.Name() }

func (a *timeoutAgent) Close() error { return a.inner.Close() }

func (a *timeoutAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if _, bounded := ctx.Deadline(); bounded {
		// An outer seam already owns the wall-clock diagnosis. The stall bound
		// still has to run here: Review and Test install a deadline before
		// calling Agent.Run through this wrapper, and skipping invokeAgent
		// would leave a progressless turn running until that 3h deadline.
		activity := observeAgentActivity(&opts)
		return invokeAgent(ctx, 0, a.stall, activity, func(runCtx context.Context) (*agent.Result, error) {
			result, err := a.inner.Run(runCtx, opts)
			cause := context.Cause(ctx)
			switch {
			case cause == nil:
				return result, err
			case err != nil:
				return nil, err
			default:
				return nil, cause
			}
		})
	}
	activity := observeAgentActivity(&opts)
	return invokeAgent(ctx, a.timeout, a.stall, activity, func(runCtx context.Context) (*agent.Result, error) {
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

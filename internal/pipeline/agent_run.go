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
	"github.com/kunchenguid/no-mistakes/internal/procreap"
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
// If the parent context already has a deadline (intent extraction, caller
// cancellation), that bound is honored and no shorter default is stacked.
// Otherwise AgentTimeout is applied as a stall budget: a silent invocation
// is cancelled there, while one still producing output or still waiting on a
// live child process may continue until it goes idle or hits
// AgentTimeoutHardCap. A late successful return after the
// deadline is rejected.
func (sctx *StepContext) RunAgent(opts agent.RunOpts) (*agent.Result, error) {
	parent := context.Background()
	if sctx != nil {
		parent = sctx.Ctx
	}
	return sctx.runAgent(parent, opts, "", 0, nil)
}

// RunAgentContext is RunAgent with an explicit parent.
func (sctx *StepContext) RunAgentContext(parent context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, "", 0, nil)
}

// RunAgentBudget is RunAgentContext with a step-specific stall budget and
// cause (Review and Test). The parent must not already carry that budget as a
// context deadline, or the activity-aware extension cannot run.
func (sctx *StepContext) RunAgentBudget(parent context.Context, timeout time.Duration, cause error, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, "", timeout, cause)
}

// RunAgentSessionContext is RunAgentSession with an explicit parent.
func (sctx *StepContext) RunAgentSessionContext(parent context.Context, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, role, 0, nil)
}

// RunAgentSessionBudget is RunAgentSessionContext with a step-specific stall
// budget and cause so a fixer turn can use review_agent_timeout without
// installing an absolute parent deadline.
func (sctx *StepContext) RunAgentSessionBudget(parent context.Context, timeout time.Duration, cause error, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, role, timeout, cause)
}

func (sctx *StepContext) runAgent(parent context.Context, opts agent.RunOpts, sessionRole SessionRole, timeout time.Duration, cause error) (*agent.Result, error) {
	var ag agent.Agent
	if timeout <= 0 {
		timeout = AgentTimeout(nil)
		if sctx != nil {
			timeout = AgentTimeout(sctx.Config)
		}
	}
	if cause == nil {
		cause = ErrAgentTimeout
	}
	if sctx != nil {
		ag = sctx.Agent
	}
	activity := observeAgentActivity(&opts)
	return invokeAgent(parent, timeout, cause, activity, func(ctx context.Context) (*agent.Result, error) {
		if sessionRole != "" && sctx != nil && sctx.Sessions != nil {
			return sctx.Sessions.Run(ctx, ag, sessionRole, opts, sctx.Log)
		}
		if ag == nil {
			return nil, errors.New("nil agent")
		}
		return ag.Run(ctx, opts)
	})
}

func invokeAgent(parent context.Context, timeout time.Duration, cause error, activity *agentActivity, run func(context.Context) (*agent.Result, error)) (*agent.Result, error) {
	ctx, cancel, budget := bindAgentDeadline(parent, timeout, cause, activity)
	result, err := run(ctx)
	runErr := classifyAgentRun(ctx, budget, activity, err)
	cancel()
	activity.finish()
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
	// exited is set once the launched subprocess reported its exit, so its
	// PID is never probed for children after the kernel may have reused it.
	exited bool
	// trackChildren is set when a stall budget applies, the only reader of
	// the child samples below.
	trackChildren bool
	// finished stops the child sampler once the invocation returned.
	finished bool
	// generation stops a sampler whose attempt or launch was superseded.
	generation int
	// current and previous are the two most recent periodic samples of the
	// launched subprocess's live descendants.
	current, previous childSample
	// helpers is frozen from a sample completed before the most recent
	// output, and is never refreshed during a quiet stretch, so a tool the
	// agent launched after speaking cannot age into it. waitingOnChild
	// counts only descendants missing from it.
	helpers childSample
}

// childSample is one read of a subprocess's live descendants.
type childSample struct {
	children map[int]bool
	ok       bool
	at       time.Time
}

const (
	// childSampleInterval paces the descendant sampler for the rest of a
	// turn; childSampleWarmupInterval samples faster right after launch,
	// when an agent starts the helpers it keeps for the whole turn (stdio
	// MCP servers, the ACP agent under acpx) just before its first output.
	childSampleInterval       = time.Second
	childSampleWarmup         = 5 * time.Second
	childSampleWarmupInterval = 100 * time.Millisecond
	// childSampleSettle is how long before an observed output a sample must
	// have completed to be frozen as helpers. Output is read a moment after
	// the agent wrote it, and a tool it announced may already be running in
	// between; a sample completed that close to the read could hold it.
	childSampleSettle = 50 * time.Millisecond
)

func newAgentActivity() *agentActivity {
	return &agentActivity{begun: time.Now()}
}

func (a *agentActivity) observe() {
	if a == nil {
		return
	}
	now := time.Now()
	a.mu.Lock()
	a.observed++
	a.last = now
	cutoff := now.Add(-childSampleSettle)
	switch {
	case !a.current.at.IsZero() && !a.current.at.After(cutoff):
		a.helpers = a.current
	case !a.previous.at.IsZero() && !a.previous.at.After(cutoff):
		a.helpers = a.previous
	default:
		a.helpers = childSample{}
	}
	a.mu.Unlock()
}

// sampleChildren reads the launched subprocess's live descendants
// periodically until the subprocess exits, the invocation returns, or a new
// attempt or launch supersedes it. Only observe() turns a sample into the
// helper baseline.
func (a *agentActivity) sampleChildren(pid, generation int, launchedAt time.Time) {
	for {
		children, err := procreap.LiveDescendants(pid)
		sample := childSample{children: children, ok: err == nil, at: time.Now()}
		a.mu.Lock()
		if generation != a.generation || a.exited || a.finished {
			a.mu.Unlock()
			return
		}
		a.previous, a.current = a.current, sample
		a.mu.Unlock()
		interval := childSampleInterval
		if time.Since(launchedAt) < childSampleWarmup {
			interval = childSampleWarmupInterval
		}
		time.Sleep(interval)
	}
}

func (a *agentActivity) resetChildren() {
	a.generation++
	a.current, a.previous, a.helpers = childSample{}, childSample{}, childSample{}
}

func (a *agentActivity) enableChildTracking() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.trackChildren = true
	a.mu.Unlock()
}

func (a *agentActivity) finish() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.finished = true
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
	a.exited = false
	a.resetChildren()
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
	a.exited = false
	a.resetChildren()
	sample := a.trackChildren && !a.finished && pid > 1
	generation, launchedAt := a.generation, a.launchedAt
	a.mu.Unlock()
	if sample {
		go a.sampleChildren(pid, generation, launchedAt)
	}
}

func (a *agentActivity) observeExit() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.exited = true
	a.mu.Unlock()
}

// waitingOnChild reports whether the launched agent subprocess is running a
// child process missing from the helper baseline frozen at its latest
// output, such as a test suite a tool call announced and then launched. Such
// a wait emits no output, so it is the only evidence that a quiet agent is
// working rather than wedged. Children already running before that output
// (an ACP agent under acpx, stdio MCP servers) live for the whole turn and
// prove nothing, and an agent that never produced output never announced a
// tool call. A missing baseline or an unreadable process table reports
// false, so the budget is never extended on a guess. It is liveness, not output: evidence() never
// reports it as the agent having produced anything.
func (a *agentActivity) waitingOnChild() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	pid := a.launchedPID
	ready := a.launched && !a.exited && a.observed > 0 && a.helpers.ok
	helpers := a.helpers.children
	a.mu.Unlock()
	if !ready {
		return false
	}
	current, err := procreap.LiveDescendants(pid)
	if err != nil {
		return false
	}
	for child := range current {
		if !helpers[child] {
			return true
		}
	}
	return false
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
			activity.observeExit()
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

// AgentTimeoutHardCap is the fail-closed bound for an invocation that is
// still producing output, or still waiting on a live child process, when the
// stall budget expires. A silent invocation
// is cancelled at the stall budget itself; this cap is the replacement bound
// so a chatty turn cannot run forever.
func AgentTimeoutHardCap(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return timeout * 2
}

// AgentTimeoutIdleGrace is how long an invocation may stay quiet after the
// stall budget before it is cancelled. Production 30m budgets use the same
// 10m quiet window AXI already treats as "this looks stalled"; shorter test
// budgets use the budget itself so tests stay fast.
func AgentTimeoutIdleGrace(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	if timeout < config.DefaultStepQuietWarning {
		return timeout
	}
	return config.DefaultStepQuietWarning
}

// agentBudget is the stall budget the shared seam applied to one invocation.
type agentBudget struct {
	timeout time.Duration
	cause   error
	start   time.Time
}

// bound names which limit cut the invocation and how long it actually ran,
// so an operator can tell a quiet turn stopped at the stall budget from a
// working one stopped at the hard cap.
func (b *agentBudget) bound(hardCap bool) string {
	ran := roundActivity(time.Since(b.start))
	if hardCap {
		return fmt.Sprintf("at its %s hard cap (twice the %s stall budget, still active; ran %s)",
			AgentTimeoutHardCap(b.timeout), b.timeout, ran)
	}
	return fmt.Sprintf("after %s (stall budget, then no recent output or live child process; ran %s)",
		b.timeout, ran)
}

// AgentBudgetBound renders which bound cut an invocation for a step's own
// timeout message: the stall budget or the hard cap, plus the elapsed time.
// Errors the shared seam did not diagnose fall back to naming timeout.
func AgentBudgetBound(err error, timeout time.Duration) string {
	var inv *agentInvocationError
	if errors.As(err, &inv) && inv.bound != "" {
		return inv.bound
	}
	return fmt.Sprintf("after %s", timeout)
}

func bindAgentDeadline(parent context.Context, timeout time.Duration, cause error, activity *agentActivity) (context.Context, context.CancelFunc, *agentBudget) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return parent, func() {}, nil
	}
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}, nil
	}
	if cause == nil {
		cause = ErrAgentTimeout
	}
	budget := &agentBudget{timeout: timeout, cause: cause, start: time.Now()}
	activity.enableChildTracking()
	hardCap := AgentTimeoutHardCap(timeout)
	idle := AgentTimeoutIdleGrace(timeout)
	cancelCtx, cancelCause := context.WithCancelCause(parent)
	ctx, deadlineCancel := context.WithDeadlineCause(cancelCtx, budget.start.Add(hardCap), cause)
	stop := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(stop)
			deadlineCancel()
			cancelCause(nil)
		})
	}
	go watchAgentDeadline(ctx, stop, timeout, idle, cause, activity, cancelCause)
	return ctx, cancel, budget
}

func watchAgentDeadline(ctx context.Context, stop <-chan struct{}, timeout, idle time.Duration, cause error, activity *agentActivity, cancelCause context.CancelCauseFunc) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			wait := activity.untilIdle(idle)
			if wait <= 0 && activity.waitingOnChild() {
				wait = idle
			}
			if wait <= 0 {
				cancelCause(cause)
				return
			}
			timer.Reset(wait)
		}
	}
}

func (a *agentActivity) untilIdle(d time.Duration) time.Duration {
	if a == nil || d <= 0 {
		return 0
	}
	a.mu.Lock()
	observed, last := a.observed, a.last
	a.mu.Unlock()
	if observed == 0 {
		return 0
	}
	remaining := d - time.Since(last)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func classifyAgentRun(ctx context.Context, budget *agentBudget, activity *agentActivity, err error) error {
	cause := context.Cause(ctx)
	if cause == nil {
		return err
	}
	// Only a budget or an inherited deadline earns a diagnosis. A plain
	// cancellation (operator abort, supersede, daemon shutdown) is already
	// self-explanatory, even when it carries a cause of its own, and must not
	// be dressed up as an agent fault. Stall-budget cancels use
	// WithCancelCause, so ctx.Err() is Canceled while Cause is the budget's;
	// hard-cap cancels are DeadlineExceeded with that same cause.
	if budget != nil && errors.Is(cause, budget.cause) {
		bound := budget.bound(errors.Is(ctx.Err(), context.DeadlineExceeded))
		if errors.Is(cause, ErrAgentTimeout) {
			return diagnoseAgentTimeout("agent timed out "+bound, bound, activity, err, cause)
		}
		// The budget belongs to the caller (a Review or Test invocation).
		// Keep its cause identity so the caller's own classifier still
		// matches, and hand it the bound plus the measurement and whatever
		// the adapter managed to say.
		return diagnoseAgentTimeout("", bound, activity, err, cause)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return diagnoseAgentTimeout("", "", activity, err, cause)
	}
	return cause
}

// diagnoseAgentTimeout builds the one error a timed-out invocation returns. It
// always carries the measured activity evidence and, crucially, whatever the
// adapter reported - a killed native agent's stderr and exit status is the only
// account of what the process was doing, and dropping it is what made this
// failure mode undiagnosable in the first place.
func diagnoseAgentTimeout(prefix, bound string, activity *agentActivity, adapterErr, cause error) error {
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
		bound:   bound,
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
	// bound is which applied budget limit cut the invocation, when one did.
	bound   string
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
		// An outer seam (RunAgent or a Review/Test invocation) already owns the
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
	return invokeAgent(ctx, a.timeout, ErrAgentTimeout, activity, func(runCtx context.Context) (*agent.Result, error) {
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

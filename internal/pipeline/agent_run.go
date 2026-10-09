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
	"github.com/kunchenguid/no-mistakes/internal/procreap"
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

// zeroCPUWedgeGrace is a short confirmation window for the measured native
// wedge shape: the process is still reported runnable, its accumulated CPU
// time remains zero, and the turn has made no progress. It is deliberately
// independent of agent_stall_timeout; that setting still owns ordinary silent
// turns and this exception only catches a process that never began executing.
const zeroCPUWedgeGrace = 5 * time.Second

// AgentTimeout is the silent-kill budget applied at the shared agent-run
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

// AgentWorkingTimeout is the optional still-working cap for the shared seam.
// Zero means the cap is unset and a turn stops at AgentTimeout.
func AgentWorkingTimeout(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 0
	}
	return cfg.AgentWorkingTimeout
}

// RunAgent executes one agent invocation with a deadline scoped only to that
// call. The parent StepContext.Ctx is left unchanged so post-agent work
// (commits, git, parsing) is not cancelled by the invocation budget.
//
// If the parent context already has a deadline (intent extraction, caller
// cancellation), that bound is honored and no shorter default is stacked.
// Otherwise AgentTimeout is the silent-kill budget. A turn still producing
// output or waiting on a live child continues only until AgentWorkingTimeout
// when that cap is set; an unset cap does not extend the turn. A late
// successful return after the deadline is rejected.
func (sctx *StepContext) RunAgent(opts agent.RunOpts) (*agent.Result, error) {
	parent := context.Background()
	if sctx != nil {
		parent = sctx.Ctx
	}
	return sctx.runAgent(parent, opts, "", 0, 0, nil)
}

// RunAgentContext is RunAgent with an explicit parent.
func (sctx *StepContext) RunAgentContext(parent context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, "", 0, 0, nil)
}

// RunAgentBudget is RunAgentContext with a step-specific silent budget,
// optional still-working cap, and cause (Review and Test). The parent must
// not already carry that budget as a context deadline, or the activity-aware
// extension cannot run. A working cap equal to timeout does not extend the
// turn.
func (sctx *StepContext) RunAgentBudget(parent context.Context, timeout, working time.Duration, cause error, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, "", timeout, working, cause)
}

// RunAgentSessionContext is RunAgentSession with an explicit parent.
func (sctx *StepContext) RunAgentSessionContext(parent context.Context, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, role, 0, 0, nil)
}

// RunAgentSessionBudget is RunAgentSessionContext with a step-specific silent
// budget and optional still-working cap so a fixer turn can use
// review_agent_timeout without installing an absolute parent deadline.
func (sctx *StepContext) RunAgentSessionBudget(parent context.Context, timeout, working time.Duration, cause error, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, role, timeout, working, cause)
}

func (sctx *StepContext) runAgent(parent context.Context, opts agent.RunOpts, sessionRole SessionRole, timeout, working time.Duration, cause error) (*agent.Result, error) {
	var ag agent.Agent
	if timeout <= 0 {
		timeout = AgentTimeout(nil)
		working = 0
		if sctx != nil {
			timeout = AgentTimeout(sctx.Config)
			working = AgentWorkingTimeout(sctx.Config)
		}
	}
	if cause == nil {
		cause = ErrAgentTimeout
	}
	if sctx != nil {
		ag = sctx.Agent
	}
	stall := AgentStallTimeout(nil)
	if sctx != nil {
		stall = AgentStallTimeout(sctx.Config)
	}
	activity := observeAgentActivity(&opts)
	return invokeAgent(parent, timeout, working, stall, cause, activity, func(ctx context.Context) (*agent.Result, error) {
		if sessionRole != "" && sctx != nil && sctx.Sessions != nil {
			return sctx.Sessions.Run(ctx, ag, sessionRole, opts, sctx.Log)
		}
		if ag == nil {
			return nil, errors.New("nil agent")
		}
		return ag.Run(ctx, opts)
	})
}

// invokeAgent combines upstream's activity-aware budget with the fork's progress bound.
func invokeAgent(parent context.Context, timeout, working, stall time.Duration, cause error, activity *agentActivity, run func(context.Context) (*agent.Result, error)) (*agent.Result, error) {
	ctx, cancelDeadline, budget := bindAgentDeadline(parent, timeout, working, cause, activity)
	runCtx, cancelRun := context.WithCancel(ctx)
	stopWatch := watchAgentStall(runCtx, cancelRun, stall, activity)
	defer cancelRun()
	defer cancelDeadline()
	defer activity.finish()
	defer func() { stopWatch() }()
	result, err := run(runCtx)
	stalled := stopWatch()
	var runErr error
	if stalled {
		runErr = diagnoseAgentStall(activity, err)
	} else {
		runErr = classifyAgentRun(ctx, budget, activity, err)
	}
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
// so detection is prompt without busy-waiting, and is capped to sample the
// zero-CPU wedge before its confirmation window expires.
func watchAgentStall(ctx context.Context, cancel context.CancelFunc, stall time.Duration, activity *agentActivity) func() bool {
	if stall <= 0 || activity == nil {
		return func() bool { return false }
	}
	interval := stall / 4
	if zeroCPUInterval := zeroCPUWedgeGrace / 2; interval > zeroCPUInterval {
		interval = zeroCPUInterval
	}
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
				if ok && (silent >= stall || activity.zeroCPURunnableWedge(silent)) {
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
	// zeroCPUSince records a continuous runnable/zero-CPU sample window.
	zeroCPUSince time.Time
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
	// agent launched after speaking, including one its first output
	// announced, cannot age into it. waitingOnChild counts only descendants
	// missing from it. It stays zero when every output so far came before
	// any sample settled, and then every live descendant counts.
	helpers childSample
}

// childSample is one read of a subprocess's live descendants, completed at
// at.
type childSample struct {
	children map[int]bool
	ok       bool
	at       time.Time
}

const (
	// childSampleInterval paces the descendant sampler.
	childSampleInterval = time.Second
	// childSampleSettle is how long before an observed output a sample must
	// have completed to be frozen as helpers. Output is read a moment after
	// the agent wrote it, and a tool it announced may already be running in
	// between; a sample completed that close to the read could hold it.
	childSampleSettle = 50 * time.Millisecond
)

func newAgentActivity() *agentActivity {
	return &agentActivity{begun: time.Now()}
}

func (a *agentActivity) zeroCPURunnableWedge(silent time.Duration) bool {
	if a == nil || silent < zeroCPUWedgeGrace {
		return false
	}
	a.mu.Lock()
	pid, launched, launchedAt := a.launchedPID, a.launched, a.launchedAt
	a.mu.Unlock()
	if !launched || pid <= 0 || time.Since(launchedAt) < zeroCPUWedgeGrace {
		return false
	}
	cpu, state, err := sampleAgentProcess(pid)
	if err != nil || cpu != 0 || len(state) == 0 || state[0] != 'R' {
		a.mu.Lock()
		a.zeroCPUSince = time.Time{}
		a.mu.Unlock()
		return false
	}
	a.mu.Lock()
	if a.zeroCPUSince.IsZero() {
		a.zeroCPUSince = time.Now()
	}
	stable := time.Since(a.zeroCPUSince) >= zeroCPUWedgeGrace
	a.mu.Unlock()
	return stable
}

func (a *agentActivity) observe() {
	if a == nil {
		return
	}
	now := time.Now()
	a.mu.Lock()
	a.observed++
	a.zeroCPUSince = time.Time{}
	a.last = now
	cutoff := now.Add(-childSampleSettle)
	switch {
	case !a.current.at.IsZero() && !a.current.at.After(cutoff):
		a.helpers = a.current
	case !a.previous.at.IsZero() && !a.previous.at.After(cutoff):
		a.helpers = a.previous
	}
	a.mu.Unlock()
}

// sampleChildren reads the launched subprocess's live descendants
// periodically until the subprocess exits, the invocation returns, or a new
// attempt or launch supersedes it. Only observe() turns a sample into the
// helper baseline.
func (a *agentActivity) sampleChildren(pid, generation int) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for range timer.C {
		children, err := procreap.LiveDescendants(pid)
		sample := childSample{children: children, ok: err == nil, at: time.Now()}
		a.mu.Lock()
		if generation != a.generation || a.exited || a.finished {
			a.mu.Unlock()
			return
		}
		a.previous, a.current = a.current, sample
		a.mu.Unlock()
		timer.Reset(childSampleInterval)
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
	a.progressed = time.Time{}
	a.progressCount = 0
	a.launched = false
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
	a.zeroCPUSince = time.Time{}
	a.exited = false
	a.resetChildren()
	sample := a.trackChildren && !a.finished && pid > 1
	generation := a.generation
	a.mu.Unlock()
	if sample {
		go a.sampleChildren(pid, generation)
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
// child process missing from the baseline frozen before its latest output,
// such as a test suite a tool call announced and then launched. Such a wait
// emits no output, so it is the only evidence that a quiet agent is working
// rather than wedged. Helpers (an ACP agent under acpx, stdio MCP servers)
// live for the whole turn and prove nothing, and an agent that never produced
// output never announced a tool call. A baseline frozen from a failed sample
// or an unreadable process table reports false, so the budget is never
// extended on a guess. It is liveness, not output: evidence() never
// reports it as the agent having produced anything.
func (a *agentActivity) waitingOnChild() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	pid := a.launchedPID
	frozen := a.helpers.ok || a.helpers.at.IsZero()
	ready := a.launched && !a.exited && a.observed > 0 && frozen
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
	a.zeroCPUSince = time.Time{}
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

// agentBudget is the silent budget and optional still-working cap the shared
// seam applied to one invocation.
type agentBudget struct {
	timeout time.Duration
	working time.Duration
	cause   error
	start   time.Time
}

// bound names which limit cut the invocation and how long it actually ran.
// With no cap the silent budget is a plain deadline that never looked at
// activity, so that label claims nothing about output or children. With a
// cap, a deadline that fired is the cap, which also cuts a quiet turn still
// inside its idle window, so that label claims no activity either; a cancel
// before the cap is the watcher finding the turn idle.
func (b *agentBudget) bound(deadlineExceeded bool) string {
	ran := roundActivity(time.Since(b.start))
	if b.working <= 0 {
		return fmt.Sprintf("after %s (silent budget; no still-working cap is set; ran %s)",
			b.timeout, ran)
	}
	if deadlineExceeded {
		return fmt.Sprintf("at its %s still-working cap (silent budget %s; ran %s)",
			b.working, b.timeout, ran)
	}
	return fmt.Sprintf("after %s (stall budget, then no recent output or live child process; ran %s)",
		b.timeout, ran)
}

// AgentBudgetBound renders which bound cut an invocation for a step's own
// timeout message: the silent budget or the still-working cap, plus the
// elapsed time.
// Errors the shared seam did not diagnose fall back to naming timeout.
func AgentBudgetBound(err error, timeout time.Duration) string {
	var inv *agentInvocationError
	if errors.As(err, &inv) && inv.bound != "" {
		return inv.bound
	}
	return fmt.Sprintf("after %s", timeout)
}

func bindAgentDeadline(parent context.Context, timeout, working time.Duration, cause error, activity *agentActivity) (context.Context, context.CancelFunc, *agentBudget) {
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
	budget := &agentBudget{timeout: timeout, working: working, cause: cause, start: time.Now()}
	if working <= timeout {
		ctx, cancel := context.WithTimeoutCause(parent, timeout, cause)
		return ctx, cancel, budget
	}
	activity.enableChildTracking()
	idle := AgentTimeoutIdleGrace(timeout)
	cancelCtx, cancelCause := context.WithCancelCause(parent)
	ctx, deadlineCancel := context.WithDeadlineCause(cancelCtx, budget.start.Add(working), cause)
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
	// still-working-cap cancels are DeadlineExceeded with that same cause.
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
	stall   time.Duration
	working time.Duration
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
		return invokeAgent(ctx, 0, 0, a.stall, ErrAgentTimeout, activity, func(runCtx context.Context) (*agent.Result, error) {
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
	return invokeAgent(ctx, a.timeout, a.working, a.stall, ErrAgentTimeout, activity, func(runCtx context.Context) (*agent.Result, error) {
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

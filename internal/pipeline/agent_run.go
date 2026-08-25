package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
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
// Otherwise AgentTimeout is applied as a silence budget: the invocation is
// cancelled only after that long with no reported activity (stdout bytes,
// native process lifecycle, or a bound adapter-native session). A late
// successful return after the deadline is rejected.
func (sctx *StepContext) RunAgent(opts agent.RunOpts) (*agent.Result, error) {
	parent := context.Background()
	if sctx != nil {
		parent = sctx.Ctx
	}
	return sctx.runAgent(parent, 0, opts, "")
}

// RunAgentContext is RunAgent with an explicit parent, used when a step has
// already installed a more specific deadline.
func (sctx *StepContext) RunAgentContext(parent context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, 0, opts, "")
}

// RunAgentBudget is RunAgentContext with an explicit silence budget in place
// of the default AgentTimeout. Steps with their own configured budget
// (review_agent_timeout, test_agent_timeout) pass it per invocation so every
// agent turn gets the full budget measured from that turn's own activity,
// never an earlier turn's remainder.
func (sctx *StepContext) RunAgentBudget(parent context.Context, budget time.Duration, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, budget, opts, "")
}

// RunAgentSessionContext is RunAgentSession with an explicit parent so a
// fixer turn can share a round budget or a per-invocation wrap.
func (sctx *StepContext) RunAgentSessionContext(parent context.Context, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, 0, opts, role)
}

// RunAgentSessionBudget is RunAgentSessionContext with an explicit silence
// budget, the session-bearing analogue of RunAgentBudget.
func (sctx *StepContext) RunAgentSessionBudget(parent context.Context, budget time.Duration, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, budget, opts, role)
}

func (sctx *StepContext) runAgent(parent context.Context, budget time.Duration, opts agent.RunOpts, sessionRole SessionRole) (*agent.Result, error) {
	var ag agent.Agent
	timeout := AgentTimeout(nil)
	if sctx != nil {
		ag = sctx.Agent
		timeout = AgentTimeout(sctx.Config)
	}
	if budget > 0 {
		timeout = budget
	}
	return invokeAgent(parent, timeout, &opts, func(ctx context.Context) (*agent.Result, error) {
		if sessionRole != "" && sctx != nil && sctx.Sessions != nil {
			return sctx.Sessions.Run(ctx, ag, sessionRole, opts, sctx.Log)
		}
		if ag == nil {
			return nil, errors.New("nil agent")
		}
		return ag.Run(ctx, opts)
	})
}

// livenessContextKey carries the invocation's liveness owner so nested seams
// (RunAgent outside, the executor's timeoutAgent backstop inside) share one
// monotonic clock instead of stacking duplicate watchdogs.
type livenessContextKey struct{}

func livenessFromContext(ctx context.Context) *invocationLiveness {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(livenessContextKey{}).(*invocationLiveness)
	return l
}

func invokeAgent(parent context.Context, timeout time.Duration, opts *agent.RunOpts, run func(context.Context) (*agent.Result, error)) (*agent.Result, error) {
	ctx, cancel, applied, liveness := bindAgentLiveness(parent, timeout)
	if liveness != nil && opts != nil {
		previous := opts.OnActivity
		opts.OnActivity = func(kind agent.ActivityKind) {
			liveness.record(kind)
			if previous != nil {
				previous(kind)
			}
		}
	}
	result, err := run(ctx)
	runErr := classifyAgentRun(ctx, applied, err)
	cancel()
	if runErr != nil {
		return nil, runErr
	}
	return result, nil
}

// bindAgentLiveness installs the invocation's silence watchdog unless an
// outer layer already owns one. Precedence: an inherited liveness owner is
// reused (single monotonic clock); an existing parent deadline is honored
// unchanged (legacy hard bound); otherwise a fresh watchdog with the given
// budget governs. The returned duration is the budget actually applied at
// this layer (0 when an outer bound governs), and the liveness is non-nil
// exactly when activity should be reported to it.
func bindAgentLiveness(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration, *invocationLiveness) {
	if parent == nil {
		parent = context.Background()
	}
	if liveness := livenessFromContext(parent); liveness != nil {
		return parent, func() {}, 0, liveness
	}
	if timeout <= 0 {
		return parent, func() {}, 0, nil
	}
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}, 0, nil
	}
	liveness := newInvocationLiveness()
	ctx, cancel := watchSilence(parent, timeout, liveness)
	return context.WithValue(ctx, livenessContextKey{}, liveness), cancel, timeout, liveness
}

func classifyAgentRun(ctx context.Context, applied time.Duration, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		if applied > 0 && errors.Is(cause, ErrAgentTimeout) {
			if ate := asAgentTimeout(cause); ate != nil {
				return ate
			}
			return fmt.Errorf("agent timed out after %s (agent silent for %s): %w", applied, applied, ErrAgentTimeout)
		}
		return cause
	}
	if err != nil {
		return err
	}
	return nil
}

// timeoutAgent is the executor backstop: every sctx.Agent.Run is bounded even
// if a future step forgets RunAgent. Nested with RunAgent it shares the outer
// invocation's liveness owner instead of stacking a second watchdog, and it
// honors an incoming context's existing deadline as before.
type timeoutAgent struct {
	inner   agent.Agent
	timeout time.Duration
}

func (a *timeoutAgent) Name() string { return a.inner.Name() }

func (a *timeoutAgent) Close() error { return a.inner.Close() }

func (a *timeoutAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return invokeAgent(ctx, a.timeout, &opts, func(runCtx context.Context) (*agent.Result, error) {
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

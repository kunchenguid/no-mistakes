package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type hangingAgent struct {
	name    string
	runFn   func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)
	calls   int
	lastCtx context.Context
}

func (h *hangingAgent) Name() string { return h.name }

func (h *hangingAgent) Close() error { return nil }

func (h *hangingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	h.calls++
	h.lastCtx = ctx
	if h.runFn != nil {
		return h.runFn(ctx, opts)
	}
	return &agent.Result{Text: "ok"}, nil
}

func TestRunAgent_HangingAgentFailsAfterTimeout(t *testing.T) {
	t.Parallel()
	ag := &hangingAgent{
		name: "hang",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Text: "late"}, nil
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 20 * time.Millisecond},
	}

	start := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if !strings.Contains(err.Error(), "agent timed out after 20ms") {
		t.Fatalf("error = %q, want timeout diagnostic", err)
	}
	if elapsed > time.Second {
		t.Fatalf("hung for %s, want a bounded fail", elapsed)
	}
}

func TestAgentTimeoutHardCapAndIdleGrace(t *testing.T) {
	t.Parallel()
	if got := AgentTimeoutHardCap(30 * time.Minute); got != 60*time.Minute {
		t.Fatalf("hard cap for 30m = %s, want 60m", got)
	}
	if got := AgentTimeoutIdleGrace(30 * time.Minute); got != config.DefaultStepQuietWarning {
		t.Fatalf("idle grace for 30m = %s, want %s", got, config.DefaultStepQuietWarning)
	}
	if got := AgentTimeoutIdleGrace(20 * time.Millisecond); got != 20*time.Millisecond {
		t.Fatalf("idle grace for 20ms = %s, want 20ms", got)
	}
}

func TestRunAgent_StreamingPastStallBudgetSucceeds(t *testing.T) {
	t.Parallel()
	const stall = 80 * time.Millisecond
	done := time.NewTimer(stall + stall/2)
	defer done.Stop()
	ag := &hangingAgent{
		name: "slow-but-working",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-done.C:
					return &agent.Result{Text: "finished after stall budget"}, nil
				case <-tick.C:
					opts.OnChunk("working\n")
				}
			}
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: stall},
	}

	result, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err != nil {
		t.Fatalf("working agent cut at the stall budget: %v", err)
	}
	if result == nil || result.Text != "finished after stall budget" {
		t.Fatalf("result = %+v, want the turn that finished after the stall budget", result)
	}
}

func TestRunAgent_SilentAgentDoesNotWaitForHardCap(t *testing.T) {
	t.Parallel()
	const stall = 250 * time.Millisecond
	ag := &hangingAgent{
		name: "mute",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: stall},
	}

	start := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if elapsed >= AgentTimeoutHardCap(stall) {
		t.Fatalf("silent agent hung for %s, want a cancel at the %s stall budget not the %s hard cap", elapsed, stall, AgentTimeoutHardCap(stall))
	}
}

func TestRunAgent_StreamingAgentHitsHardCap(t *testing.T) {
	t.Parallel()
	const stall = 40 * time.Millisecond
	ag := &hangingAgent{
		name: "never-finishes",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			tick := time.NewTicker(2 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-tick.C:
					opts.OnChunk("still going\n")
				}
			}
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: stall},
	}

	start := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if !strings.Contains(err.Error(), "last produced output") {
		t.Fatalf("error = %q, want a busy-agent diagnosis at the hard cap", err)
	}
	if !strings.Contains(err.Error(), "agent timed out at its 80ms hard cap (twice the 40ms stall budget") {
		t.Fatalf("error = %q, want the hard cap named as the bound", err)
	}
	if elapsed < stall {
		t.Fatalf("busy agent cut after %s, want to pass the %s stall budget", elapsed, stall)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("busy agent hung for %s, want a bounded hard-cap fail", elapsed)
	}
}

func TestRunAgent_LateSuccessAfterTimeoutIsRejected(t *testing.T) {
	t.Parallel()
	ag := &hangingAgent{
		name: "late",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Output: json.RawMessage(`{"summary":"should not ship"}`)}, nil
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 20 * time.Millisecond},
	}

	result, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want late-success timeout", err)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil after timeout", result)
	}
}

func TestRunAgent_SuccessfulOutputUnchanged(t *testing.T) {
	t.Parallel()
	want := json.RawMessage(`{"summary":"ok"}`)
	ag := &hangingAgent{
		name: "ok",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("successful invocation ran without a deadline")
			}
			if err := ctx.Err(); err != nil {
				t.Fatalf("live invocation context: %v", err)
			}
			return &agent.Result{Output: want, Text: "ok"}, nil
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: time.Second},
	}

	result, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || string(result.Output) != string(want) || result.Text != "ok" {
		t.Fatalf("result = %+v, want original output", result)
	}
	if sctx.Ctx.Err() != nil {
		t.Fatalf("parent context cancelled after successful run: %v", sctx.Ctx.Err())
	}
}

func TestRunAgent_HonorsExistingSoonerDeadline(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ag := &hangingAgent{
		name: "parent-deadline",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Text: "late"}, nil
		},
	}
	sctx := &StepContext{
		Ctx:    parent,
		Agent:  ag,
		Config: &config.Config{AgentTimeout: time.Hour},
	}

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err == nil {
		t.Fatal("expected parent deadline to fail the invocation")
	}
	if errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("stacked default timeout onto an existing deadline: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want parent deadline", err)
	}
}

func TestExecutor_DirectAgentRunIsDeadlineBounded(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ag := &hangingAgent{
		name: "hang",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Text: "late"}, nil
		},
	}
	step := &adaptiveCallStep{
		name: types.StepDocument,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "work"})
			return nil, err
		},
	}
	cfg := &config.Config{AgentTimeout: 20 * time.Millisecond}
	exec := NewExecutor(database, p, cfg, ag, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("expected hanging Agent.Run to fail the run")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("run status = %s, want %s", got.Status, types.RunFailed)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "agent timed out after 20ms") {
		var msg string
		if got.Error != nil {
			msg = *got.Error
		}
		t.Fatalf("run error = %q, want timeout diagnostic", msg)
	}
}

// The next three tests pin the diagnostic contract that a silent-agent timeout
// has to satisfy for an operator to act on it. Before this contract the
// invocation-timeout error printed the configured budget twice ("agent timed
// out after 30m0s (agent silent for 30m0s)") and discarded whatever the adapter
// had reported, so a wedged agent, a busy one, and a crashed one all produced
// the same undiagnosable line.

func TestRunAgent_TimeoutReportsMeasuredSilenceWhenTheAgentNeverEmits(t *testing.T) {
	t.Parallel()
	ag := &hangingAgent{
		name: "mute",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 30 * time.Millisecond},
	}

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if !strings.Contains(err.Error(), "produced no output at all") {
		t.Fatalf("error = %q, want the measured absence of output", err)
	}
	if strings.Contains(err.Error(), "silent for 30ms") {
		t.Fatalf("error = %q, must not restate the budget as if it were a measurement", err)
	}
}

func TestRunAgent_TimeoutReportsRecentOutputWhenTheAgentWasStreaming(t *testing.T) {
	t.Parallel()
	ag := &hangingAgent{
		name: "busy",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// A working agent: it streams right up to the deadline. This must
			// never be described the same way as an agent that emitted nothing.
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2 * time.Millisecond):
					opts.OnChunk("thinking\n")
				}
			}
		},
	}
	var logged strings.Builder
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 60 * time.Millisecond},
	}

	_, err := sctx.RunAgent(agent.RunOpts{
		Prompt:  "work",
		OnChunk: func(text string) { logged.WriteString(text) },
	})
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if !strings.Contains(err.Error(), "last produced output") {
		t.Fatalf("error = %q, want the measured recency of the agent's last output", err)
	}
	if strings.Contains(err.Error(), "no output at all") {
		t.Fatalf("error = %q, must not report silence for an agent that was streaming", err)
	}
	// Observation must stay an observation: the caller's own callback still runs.
	if logged.Len() == 0 {
		t.Fatal("streamed chunks did not reach the caller's OnChunk")
	}
}

func TestRunAgent_TimeoutPreservesWhatTheAdapterReported(t *testing.T) {
	t.Parallel()
	// A killed native agent's error is the only account of what its process was
	// doing - it carries the exit status and the subprocess stderr. Dropping it
	// is what left a real 30-minute failure with nothing to diagnose.
	adapterErr := errors.New("pi exited: signal: killed: pi: provider authentication required")
	ag := &hangingAgent{
		name: "native",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, adapterErr
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 20 * time.Millisecond},
	}

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "provider authentication required") {
		t.Fatalf("error = %q, want the adapter's own report preserved", err)
	}
	if !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout to stay matchable", err)
	}
	if !errors.Is(err, adapterErr) {
		t.Fatalf("error = %v, want the adapter error to stay matchable", err)
	}
}

func TestRunAgent_NativeSubprocessLivenessCountsAsObservedOutput(t *testing.T) {
	t.Parallel()
	// Adapters forward only assistant prose to OnChunk, so a long tool-using
	// turn is prose-silent while the subprocess streams events. Subprocess
	// liveness is what separates "working" from "wedged", so it has to count.
	ag := &hangingAgent{
		name: "tooling",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "tooling", Phase: agent.LifecyclePhaseStart, PID: 4242})
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2 * time.Millisecond):
					opts.OnLifecycle(agent.LifecycleEvent{Agent: "tooling", Phase: agent.LifecyclePhaseActivity})
				}
			}
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 60 * time.Millisecond},
	}

	_, err := sctx.RunAgent(agent.RunOpts{
		Prompt:      "work",
		OnLifecycle: func(agent.LifecycleEvent) {},
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "last produced output") {
		t.Fatalf("error = %q, want subprocess liveness counted as observed output", err)
	}
}

func TestRunAgent_RetryMetadataIsNotObservedOutput(t *testing.T) {
	t.Parallel()
	// Retry is adapter control metadata, not evidence that the assistant or its
	// subprocess produced output. A silent invocation that retries must retain
	// the same diagnosis as any other launched-but-mute invocation.
	ag := &hangingAgent{
		name: "retrying",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "retrying", Phase: agent.LifecyclePhaseStart, PID: 5150})
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "retrying", Phase: agent.LifecyclePhaseRetry})
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	var retries int
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 20 * time.Millisecond},
	}

	_, err := sctx.RunAgent(agent.RunOpts{
		Prompt: "work",
		OnLifecycle: func(event agent.LifecycleEvent) {
			if event.Phase == agent.LifecyclePhaseRetry {
				retries++
			}
		},
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "produced no output at all") {
		t.Fatalf("error = %q, want retry-only invocation reported as silent", err)
	}
	if strings.Contains(err.Error(), "last produced output") {
		t.Fatalf("error = %q, retry metadata must not count as output", err)
	}
	if retries != 1 {
		t.Fatalf("forwarded retry events = %d, want 1", retries)
	}
}

func TestRunAgent_FallbackResetsPriorAttemptActivity(t *testing.T) {
	t.Parallel()
	first := &hangingAgent{
		name: "active",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "active", Phase: agent.LifecyclePhaseStart, PID: 5151})
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "active", Phase: agent.LifecyclePhaseActivity})
			return nil, errors.New("active exited: unavailable")
		},
	}
	second := &hangingAgent{
		name: "silent",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "silent", Phase: agent.LifecyclePhaseStart, PID: 6161})
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  agent.NewFallback([]agent.Agent{first, second}),
		Config: &config.Config{AgentTimeout: 20 * time.Millisecond},
	}

	var lifecycleLog strings.Builder
	_, err := sctx.RunAgent(agent.RunOpts{
		Prompt: "work",
		OnLifecycle: func(event agent.LifecycleEvent) {
			if event.Message != "" {
				lifecycleLog.WriteString(event.Message)
			}
		},
	})
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if !strings.Contains(lifecycleLog.String(), "falling back to silent") {
		t.Fatalf("lifecycle log = %q, want operator-visible fallback notice", lifecycleLog.String())
	}
	if !strings.Contains(err.Error(), "produced no output at all") {
		t.Fatalf("error = %q, want silent fallback agent reported as producing no output", err)
	}
	if strings.Contains(err.Error(), "last produced output") {
		t.Fatalf("error = %q, prior attempt activity must not count as replacement output", err)
	}
	if !strings.Contains(err.Error(), "pid=6161") {
		t.Fatalf("error = %q, want silence attributed to the replacement subprocess", err)
	}
}

func TestRunAgent_PromptFormatFallbackResetsPriorAttemptActivity(t *testing.T) {
	t.Parallel()
	ag := &hangingAgent{
		name: "format-fallback",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "format-fallback", Phase: agent.LifecyclePhaseStart, PID: 8181})
			opts.OnChunk("thinking before conflict")
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "format-fallback", Phase: agent.LifecyclePhaseFallback})
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "format-fallback", Phase: agent.LifecyclePhaseStart, PID: 8282})
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 20 * time.Millisecond},
	}

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err == nil {
		t.Fatal("expected the prompt-only fallback to time out")
	}
	if !strings.Contains(err.Error(), "produced no output at all") {
		t.Fatalf("error = %q, want prompt-only fallback silence", err)
	}
	if strings.Contains(err.Error(), "last produced output") {
		t.Fatalf("error = %q, native-format output must not describe the prompt-only fallback", err)
	}
	if !strings.Contains(err.Error(), "pid=8282") {
		t.Fatalf("error = %q, want silence attributed to the prompt-only subprocess", err)
	}
}

func TestRunAgent_FallbackWithoutLaunchMeasuresSilenceFromAttempt(t *testing.T) {
	t.Parallel()
	const firstAttemptDuration = 100 * time.Millisecond
	var fallbackStarted time.Time
	first := &hangingAgent{
		name: "unavailable",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			time.Sleep(firstAttemptDuration)
			return nil, errors.New("unavailable exited: before replacement")
		},
	}
	second := &hangingAgent{
		name: "never-launched",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			fallbackStarted = time.Now()
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  agent.NewFallback([]agent.Agent{first, second}),
		Config: &config.Config{AgentTimeout: 250 * time.Millisecond},
	}

	invocationStarted := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	prefix := "produced no output at all in "
	text := err.Error()
	start := strings.Index(text, prefix)
	if start < 0 || !strings.Contains(text, "never reported a subprocess start") {
		t.Fatalf("error = %q, want measured pre-launch fallback silence", text)
	}
	durationText := strings.Fields(text[start+len(prefix):])[0]
	reported, parseErr := time.ParseDuration(durationText)
	if parseErr != nil {
		t.Fatalf("parse reported duration %q: %v", durationText, parseErr)
	}
	fallbackElapsed := time.Since(fallbackStarted)
	invocationElapsed := time.Since(invocationStarted)
	if delta := fallbackElapsed - reported; delta < -20*time.Millisecond || delta > 20*time.Millisecond {
		t.Fatalf("reported silence = %s, fallback elapsed = %s", reported, fallbackElapsed)
	}
	if reported >= invocationElapsed-firstAttemptDuration/2 {
		t.Fatalf("reported silence = %s, invocation elapsed = %s; want first attempt excluded", reported, invocationElapsed)
	}
}

func TestRunAgent_SubprocessStartAloneIsNotObservedOutput(t *testing.T) {
	t.Parallel()
	// Launching proves the binary ran, not that it is doing anything. Counting
	// the start event as output would erase the exact distinction this
	// measurement exists to expose.
	ag := &hangingAgent{
		name: "launched",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "launched", Phase: agent.LifecyclePhaseStart, PID: 777})
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 20 * time.Millisecond},
	}

	_, err := sctx.RunAgent(agent.RunOpts{
		Prompt:      "work",
		OnLifecycle: func(agent.LifecycleEvent) {},
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "produced no output at all") {
		t.Fatalf("error = %q, want a launched-but-mute agent still reported as silent", err)
	}
	if !strings.Contains(err.Error(), "pid=777") {
		t.Fatalf("error = %q, want the launched subprocess identified", err)
	}
}

func TestRunAgent_LateSubprocessLaunchMeasuresSilenceFromLaunch(t *testing.T) {
	t.Parallel()
	var launchedAt time.Time
	ag := &hangingAgent{
		name: "late-launch",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			time.Sleep(100 * time.Millisecond)
			launchedAt = time.Now()
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "late-launch", Phase: agent.LifecyclePhaseStart, PID: 888})
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: 250 * time.Millisecond},
	}

	invocationStarted := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{
		Prompt:      "work",
		OnLifecycle: func(agent.LifecycleEvent) {},
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	prefix := "produced no output at all in "
	text := err.Error()
	start := strings.Index(text, prefix)
	if start < 0 {
		t.Fatalf("error = %q, want measured subprocess silence", text)
	}
	durationText := strings.Fields(text[start+len(prefix):])[0]
	reported, parseErr := time.ParseDuration(durationText)
	if parseErr != nil {
		t.Fatalf("parse reported duration %q: %v", durationText, parseErr)
	}
	processElapsed := time.Since(launchedAt)
	invocationElapsed := time.Since(invocationStarted)
	if delta := processElapsed - reported; delta < -20*time.Millisecond || delta > 20*time.Millisecond {
		t.Fatalf("reported silence = %s, subprocess elapsed = %s", reported, processElapsed)
	}
	if reported >= invocationElapsed-50*time.Millisecond {
		t.Fatalf("reported silence = %s, invocation elapsed = %s; want late launch excluded", reported, invocationElapsed)
	}
}

func TestRunAgent_OperatorCancellationIsNotDressedUpAsAnAgentFault(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	ag := &hangingAgent{
		name: "cancelled",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := &StepContext{
		Ctx:    parent,
		Agent:  ag,
		Config: &config.Config{AgentTimeout: time.Minute},
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "produced no output") {
		t.Fatalf("error = %q, an abort must not be reported as an agent silence diagnosis", err)
	}
}

// TestExecutor_DirectAgentRunUnderACallerDeadlineRefusesLateWork pins the
// executor backstop for the one shape it exists to cover: a step that calls
// Agent.Run itself, under a context whose deadline someone else owns. The
// invocation must still not hand back work produced after that deadline.
func TestExecutor_DirectAgentRunUnderACallerDeadlineRefusesLateWork(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ag := &hangingAgent{
		name: "late",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Text: "produced after the deadline"}, nil
		},
	}
	step := &adaptiveCallStep{
		name: types.StepDocument,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			bounded, cancel := context.WithTimeout(sctx.Ctx, 20*time.Millisecond)
			defer cancel()
			result, err := sctx.Agent.Run(bounded, agent.RunOpts{Prompt: "work"})
			if err == nil {
				t.Fatalf("agent result %#v accepted after its caller's deadline expired", result)
			}
			return nil, err
		},
	}
	cfg := &config.Config{AgentTimeout: time.Hour}
	exec := NewExecutor(database, p, cfg, ag, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("expected the expired caller deadline to fail the run")
	}
}

func TestRunAgent_OperatorAbortWithACauseIsNotDressedUpAsABudgetCut(t *testing.T) {
	t.Parallel()
	aborted := errors.New(types.RunCancelReasonAbortedByUser)
	parent, cancel := context.WithCancelCause(context.Background())
	ag := &hangingAgent{
		name: "aborted",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnChunk("working\n")
			<-ctx.Done()
			return nil, errors.New("signal: killed")
		},
	}
	sctx := &StepContext{
		Ctx:    parent,
		Agent:  ag,
		Config: &config.Config{AgentTimeout: time.Minute},
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(aborted)
	}()

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if !errors.Is(err, aborted) || err.Error() != aborted.Error() {
		t.Fatalf("error = %v, want the operator's abort cause verbatim", err)
	}
	if errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, an abort must not match the agent budget", err)
	}
}

func TestRunAgent_AgentWaitingOnALiveChildOutlastsTheStallBudget(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the live-child probe reads the POSIX process table")
	}
	const stall = 2 * time.Second
	ag := &hangingAgent{
		name: "suite-runner",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// A quiet agent blocked on a long tool call: it announces the call,
			// then its child runs past the stall budget while the agent itself
			// writes nothing.
			return runLaunchedShell(ctx, opts, "read go; sleep 2.5", func(started func()) {
				opts.OnChunk("running the suite\n")
				time.Sleep(500 * time.Millisecond)
				started()
			})
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: stall},
	}

	start := time.Now()
	result, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	if err != nil {
		t.Fatalf("agent waiting on a live child cut after %s: %v", time.Since(start), err)
	}
	if result == nil || result.Text != "done" {
		t.Fatalf("result = %+v, want the finished turn", result)
	}
}

func TestRunAgent_HelperStartedJustBeforeTheFirstOutputDoesNotExtendTheBudget(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the live-child probe reads the POSIX process table")
	}
	const stall = time.Second
	ag := &hangingAgent{
		name: "hung-after-init",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// A stdio MCP server or an acpx inner agent starts moments before the
			// agent's first output, then the provider hangs.
			return runLaunchedShell(ctx, opts, "sleep 6 & read go; wait", func(func()) {
				time.Sleep(100 * time.Millisecond)
				opts.OnChunk("init\n")
			})
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: stall},
	}

	start := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if elapsed >= AgentTimeoutHardCap(stall) {
		t.Fatalf("hung agent with a helper started before its first output ran %s, want a cut before the %s hard cap", elapsed, AgentTimeoutHardCap(stall))
	}
}

func TestRunAgent_HelperStartedBeforeTheLastOutputDoesNotExtendTheBudget(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the live-child probe reads the POSIX process table")
	}
	const stall = 2 * time.Second
	ag := &hangingAgent{
		name: "wedged-with-helper",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// A long-lived helper (an ACP agent under acpx, a stdio MCP server)
			// starts with the agent, which then talks and hangs.
			return runLaunchedShell(ctx, opts, "sleep 6 & read go; wait", func(started func()) {
				time.Sleep(1200 * time.Millisecond)
				opts.OnChunk("thinking\n")
			})
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: stall},
	}

	start := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if elapsed >= AgentTimeoutHardCap(stall) {
		t.Fatalf("hung agent with a pre-output helper ran %s, want a cut before the %s hard cap", elapsed, AgentTimeoutHardCap(stall))
	}
	if !strings.Contains(err.Error(), "stall budget") {
		t.Fatalf("error = %q, want the stall budget named as the bound", err)
	}
}

func TestRunAgent_LaunchedAgentWithoutAChildIsCutAtTheStallBudget(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the live-child probe reads the POSIX process table")
	}
	const stall = time.Second
	ag := &hangingAgent{
		name: "wedged",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return runLaunchedShell(ctx, opts, "exec sleep 10", nil)
		},
	}
	sctx := &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: stall},
	}

	start := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	if elapsed >= AgentTimeoutHardCap(stall) {
		t.Fatalf("childless agent ran %s, want a cut at the %s stall budget", elapsed, stall)
	}
	if !strings.Contains(err.Error(), "stall budget") {
		t.Fatalf("error = %q, want the stall budget named as the bound", err)
	}
}

// runLaunchedShell stands in for a native adapter: it launches script as the
// agent subprocess, reports its start, runs drive while the script is alive
// (drive may call started to send the script one line on stdin), and reports
// the exit when the script ends.
func runLaunchedShell(ctx context.Context, opts agent.RunOpts, script string, drive func(started func())) (*agent.Result, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: cmd.Process.Pid})
	if drive != nil {
		drive(func() { _, _ = io.WriteString(stdin, "go\n") })
	}
	err = cmd.Wait()
	opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: cmd.Process.Pid})
	if err != nil {
		return nil, err
	}
	return &agent.Result{Text: "done"}, nil
}

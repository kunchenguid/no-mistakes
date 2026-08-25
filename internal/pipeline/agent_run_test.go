package pipeline

import (
	"context"
	"encoding/json"
	"errors"
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

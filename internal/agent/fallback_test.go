package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fallbackTestAgent struct {
	name      string
	run       func() (*Result, error)
	runCtx    func(context.Context) (*Result, error)
	calls     int
	resumable bool
}

func (a *fallbackTestAgent) Name() string { return a.name }

func (a *fallbackTestAgent) Run(ctx context.Context, _ RunOpts) (*Result, error) {
	a.calls++
	if a.runCtx != nil {
		return a.runCtx(ctx)
	}
	return a.run()
}

func (a *fallbackTestAgent) Close() error { return nil }

func (a *fallbackTestAgent) SupportsSessionResume() bool { return a.resumable }

func TestFallbackAgentFallsBackOnLaunchFailure(t *testing.T) {
	first := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return nil, errors.New(`codex start: exec: "codex": executable file not found`)
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "ok"}, nil
		},
	}
	var chunks []string

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want text ok", result)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = first %d second %d, want 1/1", first.calls, second.calls)
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "agent codex failed") || !strings.Contains(joined, "falling back to claude") {
		t.Fatalf("fallback log missing, got %q", joined)
	}
}

func TestFallbackAgentDoesNotFallBackOnFindingsResult(t *testing.T) {
	first := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return &Result{Output: []byte(`{"findings":[{"severity":"warning","description":"issue"}],"summary":"1 issue"}`)}, nil
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "should not run"}, nil
		},
	}

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if string(result.Output) == "" {
		t.Fatalf("Run() result = %+v, want findings output", result)
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("calls = first %d second %d, want 1/0", first.calls, second.calls)
	}
}

func TestFallbackAgentDoesNotFallBackOnStructuredOutputError(t *testing.T) {
	parseErr := errors.New(`codex output parse: invalid JSON (output snippet: "not json")`)
	first := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return nil, parseErr
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "should not run"}, nil
		},
	}

	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
	if !errors.Is(err, parseErr) {
		t.Fatalf("Run() error = %v, want %v", err, parseErr)
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("calls = first %d second %d, want 1/0", first.calls, second.calls)
	}
}

func TestFallbackAgent_ForwardsSessionCapability(t *testing.T) {
	first := &fallbackTestAgent{name: "codex", resumable: true, run: func() (*Result, error) { return &Result{}, nil }}
	second := &fallbackTestAgent{name: "claude", resumable: true, run: func() (*Result, error) { return &Result{}, nil }}
	if !SupportsSessionResume(NewFallback([]Agent{WithSteering(first, "/evidence"), WithSteering(second, "/evidence")})) {
		t.Fatal("fallback's primary resumable agent must retain session support")
	}
}

func TestFallbackAgent_ReportsEveryAttempt(t *testing.T) {
	first := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return nil, errors.New(`codex start: executable not found`)
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "ok"}, nil
		},
	}
	var attempts []Attempt
	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{
		OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if attempts[0].Agent != "codex" || attempts[0].Err == nil {
		t.Fatalf("first attempt = %+v", attempts[0])
	}
	if attempts[1].Agent != "claude" || attempts[1].Result == nil || attempts[1].Result.Text != "ok" {
		t.Fatalf("second attempt = %+v", attempts[1])
	}
}

func TestFallbackAgent_ExpiredPrimaryDoesNotAnnounceOrStartFallback(t *testing.T) {
	first := &fallbackTestAgent{
		name: "codex",
		runCtx: func(ctx context.Context) (*Result, error) {
			<-ctx.Done()
			return nil, errors.New("codex exited: signal: terminated")
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "must not start"}, nil
		},
	}
	var lifecycle []LifecycleEvent
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := NewFallback([]Agent{first, second}).Run(ctx, RunOpts{
		OnLifecycle: func(event LifecycleEvent) { lifecycle = append(lifecycle, event) },
	})
	if err == nil || !strings.Contains(err.Error(), "codex exited") {
		t.Fatalf("Run() error = %v, want primary timeout report preserved", err)
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("calls = first %d second %d, want 1/0", first.calls, second.calls)
	}
	for _, event := range lifecycle {
		if event.Phase == LifecyclePhaseFallback {
			t.Fatalf("announced fallback on an expired context: %+v", event)
		}
	}
}

func TestFallbackAgent_AlreadyExpiredContextStartsNoCandidate(t *testing.T) {
	first := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return &Result{Text: "must not start"}, nil
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "must not start"}, nil
		},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := NewFallback([]Agent{first, second}).Run(ctx, RunOpts{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context deadline", err)
	}
	if first.calls != 0 || second.calls != 0 {
		t.Fatalf("calls = first %d second %d, want 0/0", first.calls, second.calls)
	}
}

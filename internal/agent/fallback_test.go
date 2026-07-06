package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fallbackTestAgent struct {
	name  string
	run   func() (*Result, error)
	calls int
}

func (a *fallbackTestAgent) Name() string { return a.name }

func (a *fallbackTestAgent) Run(context.Context, RunOpts) (*Result, error) {
	a.calls++
	return a.run()
}

func (a *fallbackTestAgent) Close() error { return nil }

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

func TestFallbackAgentFallsBackOnReadOnlyUnsupported(t *testing.T) {
	first := &fallbackTestAgent{
		name: "copilot",
		run: func() (*Result, error) {
			return nil, fmt.Errorf("copilot: %w", ErrReadOnlyUnsupported)
		},
	}
	second := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return &Result{Text: "ok"}, nil
		},
	}

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{ReadOnly: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want text ok", result)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = first %d second %d, want 1/1", first.calls, second.calls)
	}
}

func TestFallbackAgentReturnsReadOnlyUnsupportedWhenNoCapableAgent(t *testing.T) {
	first := &fallbackTestAgent{
		name: "copilot",
		run: func() (*Result, error) {
			return nil, fmt.Errorf("copilot: %w", ErrReadOnlyUnsupported)
		},
	}
	second := &fallbackTestAgent{
		name: "pi",
		run: func() (*Result, error) {
			return nil, fmt.Errorf("pi: %w", ErrReadOnlyUnsupported)
		},
	}

	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{ReadOnly: true})
	if !errors.Is(err, ErrReadOnlyUnsupported) {
		t.Fatalf("Run() error = %v, want ErrReadOnlyUnsupported", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = first %d second %d, want 1/1", first.calls, second.calls)
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

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func doneResultSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"done":{"type":"boolean"}},"required":["done"]}`)
}

func TestRunWithRetry_StructuredOutputFailuresDoNotReplay(t *testing.T) {
	schema := doneResultSchema()
	tests := []struct {
		name     string
		agent    string
		classify retryClassifier
		run      func() (*Result, error)
	}{
		{
			name:     "malformed final text",
			agent:    "pi",
			classify: classifyTransient,
			run: func() (*Result, error) {
				return finalizeTextResult("pi", "{\"done\":true}\nalready complete", schema, TokenUsage{})
			},
		},
		{
			name:     "empty final text",
			agent:    "codex",
			classify: classifyTransient,
			run: func() (*Result, error) {
				return finalizeTextResult("codex", "", schema, TokenUsage{})
			},
		},
		{
			name:     "pi missing output tool",
			agent:    "pi",
			classify: classifyTransient,
			run: func() (*Result, error) {
				return nil, RejectStructuredOutput(errors.New("pi did not finish by calling no_mistakes_output alone"))
			},
		},
		{
			name:     "claude missing native output",
			agent:    "claude",
			classify: claudeRetryClassifier,
			run: func() (*Result, error) {
				return finalizeClaudeResult(&claudeResult{Subtype: "success"}, schema, TokenUsage{})
			},
		},
		{
			name:     "grok missing native output",
			agent:    "grok",
			classify: classifyTransient,
			run: func() (*Result, error) {
				return finalizeGrokResult(&Result{}, schema)
			},
		},
		{
			name:     "grok invalid native output",
			agent:    "grok",
			classify: classifyTransient,
			run: func() (*Result, error) {
				return finalizeGrokResult(&Result{Output: json.RawMessage(`{"done":"yes"}`)}, schema)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			result, err := runWithRetry(context.Background(), tc.agent, RunOpts{JSONSchema: schema}, 3, tc.classify, nil, func() (*Result, error) {
				calls++
				return tc.run()
			})
			if calls != 1 {
				t.Fatalf("completed task ran %d times, want 1", calls)
			}
			if err == nil || !IsStructuredOutputRejected(err) || !strings.Contains(err.Error(), "completed work was kept") {
				t.Fatalf("error = %v, want a kept-work structured-output failure", err)
			}
			if result == nil || result.ResumeRetryable == nil || *result.ResumeRetryable {
				t.Fatalf("ResumeRetryable = %v, want false", result)
			}
		})
	}
}

func TestFallback_CompletedFormatFailureIsNotResumeRetryable(t *testing.T) {
	schema := doneResultSchema()
	primary := &formatResultAgent{name: "primary", schema: schema}
	spare := &formatResultAgent{name: "spare", schema: schema}
	wrapped := NewFallback([]Agent{primary, spare})
	result, err := wrapped.Run(context.Background(), RunOpts{
		Session: &SessionRef{ID: "fixer-session", Agent: "primary"},
	})
	if err == nil || !IsStructuredOutputRejected(err) || !strings.Contains(err.Error(), "completed work was kept") {
		t.Fatalf("error = %v, want a kept-work structured-output failure", err)
	}
	if result == nil || result.ResumeRetryable == nil || *result.ResumeRetryable {
		t.Fatalf("ResumeRetryable = %v, want false", result)
	}
	if primary.calls != 1 || spare.calls != 0 {
		t.Fatalf("primary=%d spare=%d, want 1/0", primary.calls, spare.calls)
	}
}

type formatResultAgent struct {
	name   string
	schema json.RawMessage
	calls  int
}

func (a *formatResultAgent) Name() string                { return a.name }
func (a *formatResultAgent) SupportsSessionResume() bool { return true }
func (a *formatResultAgent) Close() error                { return nil }
func (a *formatResultAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	a.calls++
	return runWithRetry(ctx, a.name, opts, 3, classifyTransient, nil, func() (*Result, error) {
		return finalizeTextResult(a.name, "{\"done\":true}\nalready complete", a.schema, TokenUsage{})
	})
}

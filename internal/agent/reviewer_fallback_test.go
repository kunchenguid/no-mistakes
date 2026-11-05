package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReviewerQuotaFallbackKeepsReviewIndependent(t *testing.T) {
	first := &quotaTestAgent{name: "first", run: func(RunOpts) (*Result, error) {
		return nil, quotaError("first")
	}}
	second := &quotaTestAgent{name: "second", run: func(opts RunOpts) (*Result, error) {
		if opts.Prompt != "whole review" {
			t.Fatal("fallback lost the review prompt")
		}
		return &Result{Text: "reviewed"}, nil
	}}
	primary := &roleRecorder{name: "writer"}
	ag := WithReviewAgents(primary, NewFallback([]Agent{first, second}, WithReviewerQuotaFallback()), nil)
	var attempts []string
	var events []LifecycleEvent
	for _, purpose := range []string{"review", "review-coverage"} {
		result, err := ag.Run(context.Background(), RunOpts{
			Purpose: purpose, Prompt: "whole review", Session: &SessionRef{ID: "writer-session", Agent: "writer"},
			OnAttempt:   func(a Attempt) { attempts = append(attempts, a.Agent) },
			OnLifecycle: func(e LifecycleEvent) { events = append(events, e) },
		})
		if err != nil || result.Provider != "second" || result.Text != "reviewed" {
			t.Fatalf("result = %+v, error = %v", result, err)
		}
	}
	if first.calls != 1 || second.calls != 2 || len(primary.calls) != 0 {
		t.Fatalf("calls = %d/%d/%d, want 1/2/0", first.calls, second.calls, len(primary.calls))
	}
	if len(first.sessions) != 0 || len(second.sessions) != 0 || strings.Join(attempts, ",") != "first,second,second" {
		t.Fatalf("sessions = %v/%v, attempts = %v", first.sessions, second.sessions, attempts)
	}
	if len(events) != 1 || events[0].Phase != LifecyclePhaseFallback {
		t.Fatalf("events = %+v, want a fallback without changing primary quota routing", events)
	}
}

func TestReviewerQuotaFallbackExhaustionFailsWithoutRetry(t *testing.T) {
	first := &quotaTestAgent{name: "first", run: func(RunOpts) (*Result, error) { return nil, quotaError("first") }}
	second := &quotaTestAgent{name: "second", run: func(RunOpts) (*Result, error) { return nil, quotaError("second") }}
	ag := NewFallback([]Agent{first, second}, WithReviewerQuotaFallback())
	for i := 0; i < 2; i++ {
		if _, err := ag.Run(context.Background(), RunOpts{}); err == nil || !strings.Contains(err.Error(), "all explicitly configured reviewers") {
			t.Fatalf("Run error = %v, want explicit exhaustion", err)
		}
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("exhausted reviewers retried: %d/%d", first.calls, second.calls)
	}
}

func TestReviewerQuotaFallbackPreservesFailureBoundaries(t *testing.T) {
	for name, failure := range map[string]error{
		"launch":    errors.New("reviewer start: executable not found"),
		"transient": errors.New("overloaded_error"),
		"schema":    RejectStructuredOutput(quotaError("schema text")),
		"timeout":   context.DeadlineExceeded,
		"cancelled": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			first := &quotaTestAgent{name: "first", run: func(RunOpts) (*Result, error) { return nil, failure }}
			second := &roleRecorder{name: "second"}
			_, err := NewFallback([]Agent{first, second}, WithReviewerQuotaFallback()).Run(context.Background(), RunOpts{})
			if !errors.Is(err, failure) || len(second.calls) != 0 {
				t.Fatalf("error = %v, next reviewer calls = %d", err, len(second.calls))
			}
		})
	}
	t.Run("context cancelled during refusal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first := &quotaTestAgent{name: "first", run: func(RunOpts) (*Result, error) {
			cancel()
			return nil, quotaError("first")
		}}
		second := &roleRecorder{name: "second"}
		if _, err := NewFallback([]Agent{first, second}, WithReviewerQuotaFallback()).Run(ctx, RunOpts{}); err == nil || len(second.calls) != 0 {
			t.Fatalf("error = %v, next reviewer calls = %d", err, len(second.calls))
		}
	})
}

func TestReviewerQuotaFallbackHonorsOpenCodeReplaySafety(t *testing.T) {
	for name, failure := range map[string]error{
		"typed tool activity":         &opencodeMessageFailure{name: "APIError", message: "insufficient_quota", toolActivity: true},
		"wrapped typed tool activity": fmt.Errorf("review: %w", &opencodeMessageFailure{name: "APIError", message: "insufficient_quota", toolActivity: true}),
		"tool marker":                 errors.Join(quotaError("opencode"), errOpencodeToolsAlreadyRan),
		"unknown marker":              errors.Join(quotaError("opencode"), errOpencodeToolActivityUnknown),
	} {
		for _, purpose := range []string{"review", "review-coverage"} {
			t.Run(name+"/"+purpose, func(t *testing.T) {
				first := &quotaTestAgent{name: "opencode", run: func(RunOpts) (*Result, error) { return nil, failure }}
				second := &roleRecorder{name: "second"}
				ag := WithReviewAgents(&roleRecorder{name: "writer"}, NewFallback([]Agent{first, second}, WithReviewerQuotaFallback()), nil)
				_, err := ag.Run(context.Background(), RunOpts{Purpose: purpose})
				if !errors.Is(err, failure) || first.calls != 1 || len(second.calls) != 0 {
					t.Fatalf("error = %v, calls = %d/%d", err, first.calls, len(second.calls))
				}
			})
		}
	}
	t.Run("proven no tools", func(t *testing.T) {
		first := &quotaTestAgent{name: "opencode", run: func(RunOpts) (*Result, error) {
			return nil, &opencodeMessageFailure{name: "APIError", message: "insufficient_quota"}
		}}
		second := &roleRecorder{name: "second"}
		_, err := NewFallback([]Agent{first, second}, WithReviewerQuotaFallback()).Run(context.Background(), RunOpts{})
		if err != nil || len(second.calls) != 1 {
			t.Fatalf("error = %v, next calls = %d", err, len(second.calls))
		}
	})
}

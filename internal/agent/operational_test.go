package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyOperationalFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want OperationalFailureKind
		ok   bool
	}{
		{"quota exceeded", errors.New("claude error: quota exceeded"), OpFailureQuota, true},
		{"usage limit", errors.New("codex exited: usage limit reached"), OpFailureQuota, true},
		{"session limit", errors.New("session limit reached; retry later"), OpFailureQuota, true},
		{"overloaded", errors.New(`{"type":"overloaded_error"}`), OpFailureOverload, true},
		{"429 rate limit", errors.New("http 429 too many requests"), OpFailureOverload, true},
		{"503 outage", errors.New("codex exited: http 503 service_unavailable"), OpFailureOutage, true},
		{"529 outage", errors.New("upstream returned 529"), OpFailureOutage, true},
		{"auth 401", errors.New("claude error: 401 unauthorized"), OpFailureAuth, true},
		{"auth message", errors.New("authentication_error: invalid api key"), OpFailureAuth, true},
		{"missing executable", errors.New(`exec: "codex": executable file not found in $PATH`), OpFailureExecutable, true},
		{"malformed output not operational", errors.New("claude returned no structured output"), "", false},
		{"parse failure not operational", errors.New("codex output parse: invalid character"), "", false},
		{"bad patch not operational", errors.New("step review failed: findings unresolved"), "", false},
		{"nil error", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := classifyOperationalFailure(tt.err)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("classifyOperationalFailure(%v) = (%q, %v), want (%q, %v)", tt.err, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestClassifyOperationalFailureIgnoresCancellation(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, fmt.Errorf("wrapped: %w", context.Canceled)} {
		if kind, ok := classifyOperationalFailure(err); ok {
			t.Fatalf("cancellation classified as operational %q", kind)
		}
	}
}

func TestWithOperationalClassificationWrapsOnlyOperational(t *testing.T) {
	ctx := context.Background()
	quota := errors.New("quota exceeded")
	wrapped := withOperationalClassification(ctx, quota)
	var opErr *OperationalError
	if !errors.As(wrapped, &opErr) {
		t.Fatalf("operational error not wrapped: %v", wrapped)
	}
	if opErr.Kind != OpFailureQuota {
		t.Fatalf("wrapped kind = %q, want %q", opErr.Kind, OpFailureQuota)
	}
	if !errors.Is(wrapped, quota) {
		t.Fatal("wrapped error must still unwrap to the original cause")
	}

	// Non-operational errors pass through unchanged, so the pipeline still
	// sees a plain error rather than a spurious circuit-opening signal.
	plain := errors.New("claude returned no structured output")
	if got := withOperationalClassification(ctx, plain); got != plain {
		t.Fatalf("non-operational error was altered: %v", got)
	}
	if withOperationalClassification(ctx, nil) != nil {
		t.Fatal("nil error must stay nil")
	}
}

func TestWithOperationalClassificationNeverClassifiesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The killed process's exit error carries a provider-looking message, but
	// the cancelled context means this is an aborted run, not an outage.
	exitErr := errors.New("codex exited: signal: killed: http 503 service_unavailable")
	if got := withOperationalClassification(ctx, exitErr); got != exitErr {
		var opErr *OperationalError
		if errors.As(got, &opErr) {
			t.Fatalf("cancelled attempt classified as operational %q", opErr.Kind)
		}
		t.Fatalf("cancelled attempt error was altered: %v", got)
	}
}

func TestClassifyOperationalFailureIgnoresModelOutput(t *testing.T) {
	// A model's own text mentioning status codes, marked as model output, must
	// never classify as an operational provider failure.
	err := markModelOutput(errors.New(`codex output parse: invalid character (output snippet: "the API returned 401 then 429")`))
	if kind, ok := classifyOperationalFailure(err); ok {
		t.Fatalf("model-output failure classified as operational %q", kind)
	}
	if got := withOperationalClassification(context.Background(), err); got != err {
		t.Fatalf("model-output failure was wrapped: %v", got)
	}
}

func TestFinalizeTextResultParseFailureIsNonOperational(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	_, err := finalizeTextResult("codex", "the provider said 429 too many requests, retrying", schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected a schema parse failure")
	}
	if kind, ok := classifyOperationalFailure(err); ok {
		t.Fatalf("codex output-parse failure classified as operational %q", kind)
	}
}

func TestOperationalOverloadAndOutageAreRetryable(t *testing.T) {
	// Overload and outage operational signals must be retried before terminal
	// classification, so classification happens only after retries finish.
	for _, msg := range []string{"overloaded", "too many requests", "http 502 bad gateway", "gateway timeout", "service_unavailable", "http 500 internal server error", "upstream returned 504"} {
		if _, ok := classifyTransient(errors.New(msg)); !ok {
			t.Fatalf("operational overload/outage %q must be retryable", msg)
		}
	}
	// Quota, auth, and missing-executable failures cannot be helped by retrying.
	for _, msg := range []string{"quota exceeded", "401 unauthorized", "executable file not found"} {
		if _, ok := classifyTransient(errors.New(msg)); ok {
			t.Fatalf("terminal operational failure %q must not be retried", msg)
		}
	}
}

func TestClassifyTransientSkipsModelOutput(t *testing.T) {
	// Malformed model output whose snippet mentions an overload word must not
	// be retried, even though "overloaded" is otherwise a transient signal.
	err := markModelOutput(errors.New(`codex output parse: invalid character (output snippet: "the server was overloaded")`))
	if label, ok := classifyTransient(err); ok {
		t.Fatalf("model-output failure retried as transient %q", label)
	}
}

func TestClassifyOperationalFailureStatusCodes(t *testing.T) {
	cases := []struct {
		msg  string
		want OperationalFailureKind
		ok   bool
	}{
		{"claude error: api_error_status=500", OpFailureOutage, true},
		{"upstream returned 504", OpFailureOutage, true},
		{"claude error: api_error_status=529", OpFailureOutage, true},
		{"http 429 too many requests aside, code only 429", OpFailureOverload, true},
		{"claude error: api_error_status=403", OpFailureAuth, true},
		// Word boundaries keep an incidental longer number from matching a code.
		{"codex output: processed 5000 tokens", "", false},
		{"finished in 5040 ms", "", false},
	}
	for _, tc := range cases {
		if kind, ok := classifyOperationalFailure(errors.New(tc.msg)); ok != tc.ok || kind != tc.want {
			t.Fatalf("classifyOperationalFailure(%q) = (%q, %v), want (%q, %v)", tc.msg, kind, ok, tc.want, tc.ok)
		}
	}
}

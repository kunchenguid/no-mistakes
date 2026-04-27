package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClassifyTransient_Positive(t *testing.T) {
	cases := []struct {
		name    string
		errMsg  string
		wantSub string // substring of label
	}{
		{
			name:    "claude overloaded_error stderr",
			errMsg:  `claude exited: exit status 1: API Error: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantSub: "overloaded",
		},
		{
			name:    "rate_limit_error",
			errMsg:  `claude exited: exit status 1: rate_limit_error: too many requests`,
			wantSub: "rate_limit",
		},
		{
			name:    "http 429 in body",
			errMsg:  `POST https://api.anthropic.com/v1/messages failed with 429: rate limited`,
			wantSub: "429",
		},
		{
			name:    "http 503",
			errMsg:  `GET /v3/stream_chat failed with 503: service unavailable`,
			wantSub: "503",
		},
		{
			name:    "http 529 anthropic overloaded",
			errMsg:  `claude exited: exit status 1: HTTP 529 from api.anthropic.com`,
			wantSub: "529",
		},
		{
			name:    "connection refused",
			errMsg:  `dial tcp 127.0.0.1:5555: connection refused`,
			wantSub: "connection refused",
		},
		{
			name:    "connection reset",
			errMsg:  `read tcp 1.2.3.4:80: connection reset by peer`,
			wantSub: "connection reset",
		},
		{
			name:    "io timeout",
			errMsg:  `Post "https://api.anthropic.com/v1/messages": context deadline exceeded (i/o timeout)`,
			wantSub: "i/o timeout",
		},
		{
			name:    "no such host",
			errMsg:  `dial tcp: lookup api.anthropic.com: no such host`,
			wantSub: "dns",
		},
		{
			name:    "tls handshake",
			errMsg:  `Post "https://api.anthropic.com": net/http: TLS handshake timeout`,
			wantSub: "tls",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, ok := classifyTransient(errors.New(tc.errMsg))
			if !ok {
				t.Fatalf("expected transient classification for %q", tc.errMsg)
			}
			if !strings.Contains(strings.ToLower(label), strings.ToLower(tc.wantSub)) {
				t.Errorf("label %q does not contain %q", label, tc.wantSub)
			}
		})
	}
}

func TestClassifyTransient_Negative(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
	}{
		{
			name:   "auth failure",
			errMsg: `claude exited: exit status 1: API Error: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
		},
		{
			name:   "invalid request",
			errMsg: `claude exited: exit status 1: API Error: {"type":"error","error":{"type":"invalid_request_error","message":"Bad Request"}}`,
		},
		{
			name:   "missing structured output",
			errMsg: `claude returned no structured output`,
		},
		{
			name:   "permission denied",
			errMsg: `permission denied`,
		},
		{
			name:   "schema validation",
			errMsg: `JSON output missing required field "summary"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if label, ok := classifyTransient(errors.New(tc.errMsg)); ok {
				t.Errorf("expected non-transient for %q, got %q", tc.errMsg, label)
			}
		})
	}
}

func TestClassifyTransient_NilAndContext(t *testing.T) {
	if _, ok := classifyTransient(nil); ok {
		t.Error("nil error should not be transient")
	}
	if _, ok := classifyTransient(context.Canceled); ok {
		t.Error("context.Canceled should not be transient")
	}
	if _, ok := classifyTransient(context.DeadlineExceeded); ok {
		t.Error("context.DeadlineExceeded should not be transient")
	}
}

// withFastBackoff swaps transientBackoff with a near-instant version that
// preserves ctx-cancel semantics. Returns a restore func.
func withFastBackoff(t *testing.T) func() {
	t.Helper()
	prev := transientBackoff
	transientBackoff = func(ctx context.Context, attempt int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
			return nil
		}
	}
	return func() { transientBackoff = prev }
}

func TestRunWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	defer withFastBackoff(t)()

	calls := 0
	transientErr := fmt.Errorf("API Error: overloaded_error: please retry")
	var chunks []string
	opts := RunOpts{OnChunk: func(s string) { chunks = append(chunks, s) }}

	res, err := runWithRetry(context.Background(), "claude", opts, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		if calls < 3 {
			return nil, transientErr
		}
		return &Result{Text: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if res == nil || res.Text != "ok" {
		t.Fatalf("expected result text 'ok', got %+v", res)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (2 retries + success), got %d", calls)
	}
	if len(chunks) < 2 {
		t.Errorf("expected at least 2 retry notification chunks, got %d: %v", len(chunks), chunks)
	}
	for i, c := range chunks {
		if !strings.Contains(strings.ToLower(c), "transient") || !strings.Contains(strings.ToLower(c), "overloaded") {
			t.Errorf("chunk[%d]=%q should mention 'transient' and the classification label", i, c)
		}
	}
}

func TestRunWithRetry_CallsRetryRecoveryBeforeRetry(t *testing.T) {
	defer withFastBackoff(t)()

	calls := 0
	recovered := false
	_, err := runWithRetry(context.Background(), "opencode", RunOpts{}, 1, classifyTransient, func(label string) {
		if label == "connection refused" {
			recovered = true
		}
	}, func() (*Result, error) {
		calls++
		if !recovered {
			return nil, errors.New("dial tcp 127.0.0.1:5555: connection refused")
		}
		return &Result{Text: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("expected success after retry recovery, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected one retry after recovery, got %d calls", calls)
	}
	if !recovered {
		t.Fatal("expected retry recovery to run")
	}
}

func TestRunWithRetry_PermanentErrorFailsImmediately(t *testing.T) {
	defer withFastBackoff(t)()

	calls := 0
	permErr := errors.New("API Error: authentication_error: invalid x-api-key")

	_, err := runWithRetry(context.Background(), "claude", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		return nil, permErr
	})
	if err == nil {
		t.Fatal("expected permanent error to propagate")
	}
	if calls != 1 {
		t.Errorf("expected single call for permanent error, got %d", calls)
	}
}

func TestRunWithRetry_ExhaustsRetries(t *testing.T) {
	defer withFastBackoff(t)()

	calls := 0
	transientErr := errors.New("503 service unavailable")

	_, err := runWithRetry(context.Background(), "claude", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		return nil, transientErr
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 4 { // 1 initial + 3 retries
		t.Errorf("expected 4 calls (1 + 3 retries), got %d", calls)
	}
}

func TestRunWithRetry_RespectsContextCancellation(t *testing.T) {
	// Use a real backoff that would normally take ~1s, but cancel ctx
	// before the first sleep finishes to confirm short-circuit.
	prev := transientBackoff
	transientBackoff = func(ctx context.Context, attempt int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}
	defer func() { transientBackoff = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	transientErr := errors.New("overloaded_error")
	calls := 0

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := runWithRetry(ctx, "claude", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		return nil, transientErr
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("expected fast cancel, took %v", elapsed)
	}
	if calls > 2 {
		t.Errorf("expected at most 2 calls before cancel, got %d", calls)
	}
}

func TestRunWithRetry_CombinedClassifierForClaude(t *testing.T) {
	defer withFastBackoff(t)()

	// claudeRetryClassifier should retry both transient API errors AND errNoStructuredOutput.
	calls := 0
	_, err := runWithRetry(context.Background(), "claude", RunOpts{}, 3, claudeRetryClassifier, nil, func() (*Result, error) {
		calls++
		return nil, errNoStructuredOutput
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 4 {
		t.Errorf("expected 4 calls for errNoStructuredOutput retries, got %d", calls)
	}
	if !errors.Is(err, errNoStructuredOutput) {
		t.Errorf("expected errNoStructuredOutput to surface as final error, got %v", err)
	}
}

func TestTransientBackoffDuration_Progression(t *testing.T) {
	// Without jitter, progression should be base * 4^(attempt-1).
	base := time.Second
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, base},
		{2, 4 * base},
		{3, 16 * base},
	} {
		got := transientBackoffBaseDuration(tc.attempt, base)
		if got != tc.want {
			t.Errorf("attempt %d: want %v, got %v", tc.attempt, tc.want, got)
		}
	}
}

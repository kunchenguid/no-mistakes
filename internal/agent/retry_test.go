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
			// The exact failure shape behind the 2026-09-27 EAI_AGAIN incident:
			// the CLI's plain-text banner on stdout, an empty stderr, and no
			// result event carrying api_error_status.
			name:    "claude stdout EAI_AGAIN banner",
			errMsg:  `claude exited: exit status 1: stdout: API Error: Can't reach the API server — check your internet or DNS (EAI_AGAIN)`,
			wantSub: "dns",
		},
		{
			name:    "node dns temporary failure errno",
			errMsg:  `claude exited: exit status 1: getaddrinfo EAI_AGAIN api.anthropic.com`,
			wantSub: "dns",
		},
		{
			name:    "node dns notfound errno",
			errMsg:  `codex exited: exit status 1: getaddrinfo ENOTFOUND api.anthropic.com`,
			wantSub: "dns",
		},
		{
			name:    "node connection refused errno",
			errMsg:  `codex exited: exit status 1: connect ECONNREFUSED 104.18.32.47:443`,
			wantSub: "connection refused",
		},
		{
			name:    "node connection reset errno",
			errMsg:  `pi exited: exit status 1: read ECONNRESET`,
			wantSub: "connection reset",
		},
		{
			name:    "node connect timeout errno",
			errMsg:  `grok exited: exit status 1: connect ETIMEDOUT 1.2.3.4:443`,
			wantSub: "timeout",
		},
		{
			name:    "tls handshake",
			errMsg:  `Post "https://api.anthropic.com": net/http: TLS handshake timeout`,
			wantSub: "tls",
		},
		{
			name:    "prose turn ending",
			errMsg:  `antigravity output parse: ended its turn with prose instead of the required JSON object`,
			wantSub: "prose",
		},
		{
			name:    "agy permission declaration",
			errMsg:  `antigravity reported error: declaring permissions: cortex tool view_file: failed`,
			wantSub: "permission",
		},
		{
			name:    "invalid tool call",
			errMsg:  `antigravity reported error: invalid tool call error (invalid_args)`,
			wantSub: "tool call",
		},
		{
			name:    "provider protocol residue after JSON",
			errMsg:  `pi output parse: invalid character '<' after top-level value (output snippet: "{\"summary\":\"x\"}")`,
			wantSub: "protocol residue",
		},
		{
			name:    "protocol residue with provider-like output text",
			errMsg:  `pi output parse: invalid character '<' after top-level value (output snippet: "{\"balance\":\"token\"}")`,
			wantSub: "protocol residue",
		},
		{
			name:    "pi missing output tool",
			errMsg:  `pi did not finish by calling no_mistakes_output alone`,
			wantSub: "missing output tool",
		},
		{
			name:    "pi missing output tool with benign stderr",
			errMsg:  `pi did not finish by calling no_mistakes_output alone: transient provider response`,
			wantSub: "missing output tool",
		},
		{
			name:    "pi fetch failed",
			errMsg:  `pi exited: exit status 1: fetch failed`,
			wantSub: "fetch failed",
		},
		{
			name:    "unfused split bare JSON objects",
			errMsg:  `pi output parse: split bare JSON objects could not be fused into one valid object`,
			wantSub: "split",
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
		{
			name:   "pi model access unauthorized",
			errMsg: `pi exited: unable to verify model access: 401 unauthorized`,
		},
		{
			name:   "pi model access denied",
			errMsg: `pi exited: unable to verify model access: 403 forbidden`,
		},
		{
			name:   "pi missing output unauthorized",
			errMsg: `pi did not finish by calling no_mistakes_output alone: unable to verify model access: 401 unauthorized`,
		},
		{
			name:   "pi missing output permission denied",
			errMsg: `pi did not finish by calling no_mistakes_output alone: permission denied`,
		},
		{
			name:   "pi missing output forbidden",
			errMsg: `pi did not finish by calling no_mistakes_output alone: unable to verify model access: 403 forbidden`,
		},
		{
			name:   "pi schema validation",
			errMsg: `pi output parse: JSON output missing required field "summary"`,
		},
		{
			name:   "pi fetch quota exhausted",
			errMsg: `pi exited: fetch failed: quota exhausted`,
		},
		{
			name:   "pi fetch permission denied",
			errMsg: `pi exited: exit status 1: fetch failed: permission denied`,
		},
		{
			name:   "pi fetch unauthorized",
			errMsg: `pi exited: exit status 1: fetch failed: 401 unauthorized`,
		},
		{
			name:   "pi missing output quota exhausted",
			errMsg: `pi did not finish by calling no_mistakes_output alone: insufficient_quota`,
		},
		{
			name:   "generic fetch failure",
			errMsg: `fetch failed`,
		},
		{
			name:   "competing verdicts",
			errMsg: `pi output parse: multiple bare JSON objects found in output`,
		},
		{
			name:   "free usage limit",
			errMsg: `API rate limit reached with HTTP 429: FreeUsageLimit exceeded`,
		},
		{
			name:   "quota exhausted",
			errMsg: `provider error: quota_exhausted`,
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

func TestRunWithRetry_DoesNotRetryQuotaExhaustion(t *testing.T) {
	defer withFastBackoff(t)()

	for _, message := range []string{
		"API rate limit reached with HTTP 429: FreeUsageLimit exceeded",
		"API request failed with HTTP 429: insufficient_quota",
		"API request failed with HTTP 429: You exceeded your current quota",
	} {
		t.Run(message, func(t *testing.T) {
			calls := 0
			quotaErr := errors.New(message)
			_, err := runWithRetry(context.Background(), "antigravity", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
				calls++
				return nil, quotaErr
			})
			if !errors.Is(err, quotaErr) {
				t.Fatalf("expected quota error to propagate, got %v", err)
			}
			if calls != 1 {
				t.Fatalf("expected one call for quota exhaustion, got %d", calls)
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
	for _, message := range []string{
		"pi did not finish by calling no_mistakes_output alone",
		"pi exited: exit status 1: fetch failed",
	} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			if label, ok := classifyTransient(fmt.Errorf("%s: %w", message, cause)); ok {
				t.Errorf("%s wrapping %v must remain terminal, got %q", message, cause, label)
			}
		}
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
	var attempts []Attempt
	opts := RunOpts{
		OnChunk:   func(s string) { chunks = append(chunks, s) },
		OnAttempt: func(attempt Attempt) { attempts = append(attempts, attempt) },
	}

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
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(attempts))
	}
	for i, attempt := range attempts {
		if attempt.Agent != "claude" || attempt.StartedAt.IsZero() || attempt.CompletedAt.IsZero() {
			t.Fatalf("attempt %d = %+v", i, attempt)
		}
		if i < 2 && attempt.Err == nil {
			t.Fatalf("attempt %d must preserve the transient error", i)
		}
	}
	if attempts[2].Result == nil || attempts[2].Result.Text != "ok" {
		t.Fatalf("successful attempt = %+v", attempts[2])
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

func TestRunWithRetry_PiTransientThenSucceeds(t *testing.T) {
	previous := transientBackoff
	transientBackoff = func(ctx context.Context, _ int) error { return ctx.Err() }
	t.Cleanup(func() { transientBackoff = previous })

	for _, message := range []string{
		"pi did not finish by calling no_mistakes_output alone",
		"pi exited: exit status 1: fetch failed",
	} {
		t.Run(message, func(t *testing.T) {
			calls := 0
			var events []LifecycleEvent
			var attempts []Attempt
			firstErr := errors.New(message)
			want := &Result{Text: "ok"}
			opts := RunOpts{
				OnLifecycle: func(e LifecycleEvent) { events = append(events, e) },
				OnAttempt:   func(a Attempt) { attempts = append(attempts, a) },
			}
			got, err := runWithRetry(context.Background(), "pi", opts, 1, classifyTransient, nil, func() (*Result, error) {
				calls++
				if calls == 1 {
					return nil, firstErr
				}
				return want, nil
			})
			if err != nil || got != want || calls != 2 {
				t.Fatalf("result = %v, error = %v, calls = %d; want success on second attempt", got, err, calls)
			}
			if len(attempts) != 2 || attempts[0].Err != firstErr || attempts[1].Result != want {
				t.Fatalf("attempt telemetry = %+v", attempts)
			}
			if len(events) != 1 || events[0].Agent != "pi" || events[0].Phase != LifecyclePhaseRetry || !strings.Contains(events[0].Message, "attempt 2/2") {
				t.Fatalf("retry telemetry = %+v", events)
			}
		})
	}
}

func TestRunWithRetry_ProtocolResidueTicket401StillRetries(t *testing.T) {
	previous := transientBackoff
	transientBackoff = func(ctx context.Context, _ int) error { return ctx.Err() }
	t.Cleanup(func() { transientBackoff = previous })

	calls := 0
	residueErr := errors.New(`pi output parse: invalid character '<' after top-level value (output snippet: "{\"summary\":\"ticket 401\"}<html>")`)
	result, err := runWithRetry(context.Background(), "pi", RunOpts{}, 1, classifyTransient, nil, func() (*Result, error) {
		calls++
		if calls == 1 {
			return nil, residueErr
		}
		return &Result{Text: "ok"}, nil
	})
	if err != nil || result == nil || result.Text != "ok" {
		t.Fatalf("result = %+v, error = %v; want retry success", result, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want one retry", calls)
	}
}

func TestRunWithRetry_EmitsRetryLifecycleWhenConfigured(t *testing.T) {
	defer withFastBackoff(t)()

	calls := 0
	var chunks []string
	var events []LifecycleEvent
	opts := RunOpts{
		OnChunk:     func(s string) { chunks = append(chunks, s) },
		OnLifecycle: func(e LifecycleEvent) { events = append(events, e) },
	}

	_, err := runWithRetry(context.Background(), "codex", opts, 1, classifyTransient, nil, func() (*Result, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("API Error: 503 overloaded")
		}
		return &Result{Text: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %v, want one retry lifecycle event", events)
	}
	if events[0].Agent != "codex" || events[0].Phase != LifecyclePhaseRetry {
		t.Fatalf("event = %+v, want codex retry", events[0])
	}
	if !strings.Contains(events[0].Message, "attempt 2/2") || !strings.Contains(events[0].Message, "503") {
		t.Fatalf("retry message = %q, want attempt and label", events[0].Message)
	}
	if len(chunks) != 0 {
		t.Fatalf("OnChunk should not duplicate retry lifecycle events, got %v", chunks)
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
	if !IsTerminalProviderError(err) || !errors.Is(err, transientErr) || err.Error() != transientErr.Error() {
		t.Fatalf("exhaustion must be terminal without losing the original error: %v", err)
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

func TestClaudeRetryClassifier_ReportedAPIErrorDecidesAlone(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, tc := range []struct {
		name      string
		err       error
		wantLabel string
		wantRetry bool
	}{
		{
			name:      "transient status",
			err:       claudeExitError(exit, "", &claudeAPIError{Status: 503, Message: "API Error: 503"}, ""),
			wantLabel: "http 503",
			wantRetry: true,
		},
		{
			name: "exhausted quota behind a transient status",
			err:  claudeExitError(exit, "", &claudeAPIError{Status: 429, Message: "API Error: 429 You exceeded your current quota"}, ""),
		},
		{
			name: "exhausted quota behind the displayed message cut",
			err:  claudeExitError(exit, "", &claudeAPIError{Status: 429, Message: strings.Repeat("x", claudeAPIErrorMessageMaxRunes+50) + " quota exceeded"}, ""),
		},
		{
			name: "permanent status beside transient-looking stderr",
			err:  claudeExitError(exit, "upstream proxy returned 503", &claudeAPIError{Status: 400, Message: "API Error: 400"}, ""),
		},
		{
			name: "cancelled invocation",
			err:  claudeExitError(context.Canceled, "", &claudeAPIError{Status: 529, Message: "API Error: 529"}, ""),
		},
		{
			name: "authentication status outranks timeout text",
			err:  claudeExitError(exit, "", &claudeAPIError{Status: 401, Message: "Request timed out"}, ""),
		},
		{
			name: "usage limit outranks timeout text",
			err:  claudeExitError(exit, "", &claudeAPIError{Message: "Request timed out", usageLimitReached: true}, ""),
		},
		{
			name: "quota diagnostic outranks timeout text",
			err:  claudeExitError(exit, "", &claudeAPIError{Message: "Request timed out: quota exceeded"}, ""),
		},
		{
			name: "cancellation outranks timeout text",
			err:  claudeExitError(context.Canceled, "", &claudeAPIError{Message: "Request timed out"}, ""),
		},
		{
			name: "deadline outranks timeout text",
			err:  claudeExitError(context.DeadlineExceeded, "", &claudeAPIError{Message: "Request timed out"}, ""),
		},
		{
			name:      "stderr alone",
			err:       claudeExitError(exit, "HTTP 529 from api.anthropic.com", nil, ""),
			wantLabel: "http 529",
			wantRetry: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label, retry := claudeRetryClassifier(tc.err)
			if retry != tc.wantRetry || label != tc.wantLabel {
				t.Errorf("claudeRetryClassifier(%q) = (%q, %v), want (%q, %v)", tc.err, label, retry, tc.wantLabel, tc.wantRetry)
			}
		})
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

func TestRunWithRetry_SkipsRetryWhenFirstAttemptConsumedTheBudget(t *testing.T) {
	defer withFastBackoff(t)()

	const deadline = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	transientErr := fmt.Errorf("API Error: overloaded_error: please retry")
	calls := 0
	_, err := runWithRetry(ctx, "claude", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		time.Sleep(400 * time.Millisecond)
		return nil, transientErr
	})
	if calls != 1 {
		t.Fatalf("calls = %d, want 1: a retry may not start with less budget than the failed attempt took", calls)
	}
	if !errors.Is(err, transientErr) {
		t.Fatalf("err = %v, want it to wrap the original error", err)
	}
	if !strings.Contains(err.Error(), "retry skipped") || !strings.Contains(err.Error(), "step budget") {
		t.Fatalf("err = %q, want a skipped-for-budget message", err)
	}
	if !IsTerminalProviderError(err) {
		t.Fatal("budget-skipped adapter exhaustion must be terminal")
	}
}

func TestRunWithRetry_SkipsRetryWhenBackoffConsumesTheRemainingBudget(t *testing.T) {
	prev := transientBackoff
	transientBackoff = func(ctx context.Context, attempt int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
			return nil
		}
	}
	defer func() { transientBackoff = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	transientErr := fmt.Errorf("API Error: overloaded_error: please retry")
	calls := 0
	_, err := runWithRetry(ctx, "claude", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		time.Sleep(200 * time.Millisecond)
		return nil, transientErr
	})
	if calls != 1 {
		t.Fatalf("calls = %d, want 1: the backoff left less budget than the failed attempt took", calls)
	}
	if !errors.Is(err, transientErr) || !strings.Contains(err.Error(), "retry skipped") {
		t.Fatalf("err = %v, want a skipped-for-budget error wrapping the original", err)
	}
	if !IsTerminalProviderError(err) {
		t.Fatal("backoff-budget-skipped adapter exhaustion must be terminal")
	}
}

func TestRunWithRetry_RetriesFastFailureUnderShortDeadline(t *testing.T) {
	defer withFastBackoff(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	calls := 0
	res, err := runWithRetry(ctx, "claude", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		if calls < 2 {
			return nil, fmt.Errorf("API Error: overloaded_error: please retry")
		}
		return &Result{Text: "ok"}, nil
	})
	if err != nil || res == nil || res.Text != "ok" || calls != 2 {
		t.Fatalf("res=%+v err=%v calls=%d, want a successful second attempt", res, err, calls)
	}
}

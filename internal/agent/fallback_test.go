package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fallbackTestAgent struct {
	name        string
	run         func() (*Result, error)
	runWithOpts func(RunOpts) (*Result, error)
	calls       int
	resumable   bool
}

func (a *fallbackTestAgent) Name() string { return a.name }

func (a *fallbackTestAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	a.calls++
	if a.runWithOpts != nil {
		return a.runWithOpts(opts)
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
	parseErr := errors.New(`codex output parse: invalid JSON (output snippet: "rate limit")`)
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

func TestClassifyProviderErrorSanitizesLifecycleDiagnostics(t *testing.T) {
	const secretDiagnostic = "weekly limit reached; token=raw-provider-secret"
	classified := ClassifyProviderError(errors.New("provider failed: "+secretDiagnostic), secretDiagnostic)

	var event LifecycleEvent
	emitAgentExited(RunOpts{OnLifecycle: func(got LifecycleEvent) { event = got }}, "claude", 42, classified)
	if strings.Contains(classified.Error(), secretDiagnostic) || strings.Contains(event.Message, secretDiagnostic) {
		t.Fatalf("classified quota diagnostic leaked: error=%q event=%q", classified, event.Message)
	}
	if !strings.Contains(classified.Error(), "session/quota limit") {
		t.Fatalf("classified error = %q, want bounded quota reason", classified)
	}

	generic := errors.New("provider failed: diagnostic retained")
	emitAgentExited(RunOpts{OnLifecycle: func(got LifecycleEvent) { event = got }}, "claude", 42, ClassifyProviderError(generic, "diagnostic retained"))
	if !strings.Contains(event.Message, generic.Error()) {
		t.Fatalf("generic lifecycle event = %q, want original diagnostic", event.Message)
	}
}

func TestFallbackAgentFallsBackOnExplicitQuotaExit(t *testing.T) {
	first := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			err := errors.New("claude exited: exit status 1: You've hit your session limit")
			return nil, ClassifyProviderError(err, "You've hit your session limit")
		},
	}
	second := &fallbackTestAgent{
		name: "pi",
		run: func() (*Result, error) {
			return &Result{Text: "recovered"}, nil
		},
	}

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "recovered" || result.Provider != "pi" {
		t.Fatalf("Run() result = %+v, want pi recovery", result)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = first %d second %d, want 1/1", first.calls, second.calls)
	}
}

func TestFallbackAgentIgnoresAssistantQuotaLanguage(t *testing.T) {
	invocationErr := errors.New("provider returned invalid response")
	first := &fallbackTestAgent{
		name: "claude",
		runWithOpts: func(opts RunOpts) (*Result, error) {
			opts.OnChunk("The review mentions a rate limit")
			return nil, invocationErr
		},
	}
	second := &fallbackTestAgent{
		name: "pi",
		run:  func() (*Result, error) { return &Result{Text: "must not run"}, nil },
	}

	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{OnChunk: func(string) {}})
	if !errors.Is(err, invocationErr) {
		t.Fatalf("Run() error = %v, want original invocation error", err)
	}
	if second.calls != 0 {
		t.Fatalf("fallback calls = %d, want 0", second.calls)
	}
}

func TestFallbackAgentReportsAllQuotaExhaustionWithoutSecrets(t *testing.T) {
	first := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			err := errors.New("claude exited: exit status 1: weekly limit reached; token=secret-one")
			return nil, ClassifyProviderError(err, "weekly limit reached; token=secret-one")
		},
	}
	second := &fallbackTestAgent{
		name: "pi",
		run: func() (*Result, error) {
			err := errors.New("pi exited: exit status 1: rate_limit_error; token=secret-two")
			return nil, ClassifyProviderError(err, "rate_limit_error; token=secret-two")
		},
	}

	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
	if !IsQuotaFallbackError(err) {
		t.Fatalf("error = %v, want quota fallback error", err)
	}
	message := err.Error()
	for _, want := range []string{"claude", "pi", "session/quota limit", "rate limit"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
	for _, secret := range []string{"secret-one", "secret-two"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked secret %q: %s", secret, message)
		}
	}
}

func TestFallbackAgentQuotaExhaustionBoundsAttemptsAndAgentNames(t *testing.T) {
	const agentCount = maxQuotaFallbackAttempts + 4
	agents := make([]Agent, 0, agentCount)
	for i := 0; i < agentCount; i++ {
		agents = append(agents, &fallbackTestAgent{
			name: strings.Repeat(string(rune('a'+i)), maxQuotaFallbackAgentNameRunes+20),
			run: func() (*Result, error) {
				err := errors.New("provider rate limit exceeded; token=secret")
				return nil, ClassifyProviderError(err, "provider rate limit exceeded; token=secret")
			},
		})
	}

	_, err := NewFallback(agents).Run(context.Background(), RunOpts{})
	var quotaErr *quotaFallbackError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("error = %v, want quota fallback error", err)
	}
	if got := len(quotaErr.attempts); got != maxQuotaFallbackAttempts {
		t.Fatalf("recorded attempts = %d, want %d", got, maxQuotaFallbackAttempts)
	}
	if !quotaErr.omitted {
		t.Fatal("quota fallback error did not record omitted attempts")
	}
	for _, attempt := range quotaErr.attempts {
		if got := len([]rune(attempt.agent)); got > maxQuotaFallbackAgentNameRunes+len([]rune(quotaFallbackTruncationMarker)) {
			t.Fatalf("recorded agent name length = %d, want bounded name", got)
		}
	}
	message := err.Error()
	if !strings.Contains(message, quotaFallbackAttemptsOmittedMarker) {
		t.Fatalf("error = %q, want omitted-attempt marker", message)
	}
	if strings.Contains(message, "secret") {
		t.Fatalf("error leaked provider diagnostic: %q", message)
	}
	if len(message) > 1024 {
		t.Fatalf("error length = %d, want fixed bound", len(message))
	}
}

func TestFallbackAgentSingletonSanitizesQuotaExhaustion(t *testing.T) {
	only := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			err := errors.New("claude exited: weekly limit reached; token=secret")
			return nil, ClassifyProviderError(err, "weekly limit reached; token=secret")
		},
	}

	_, err := NewFallback([]Agent{only}).Run(context.Background(), RunOpts{})
	if !IsQuotaFallbackError(err) {
		t.Fatalf("error = %v, want quota fallback error", err)
	}
	if strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "claude (session/quota limit)") {
		t.Fatalf("error = %q, want bounded classification without diagnostic", err)
	}
}

func TestFallbackAgentPreservesGenericErrorAfterQuotaTransition(t *testing.T) {
	quota := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return nil, ClassifyProviderError(errors.New("quota detail"), "rate_limit_error")
		},
	}
	terminal := errors.New("structured output validation failed")
	generic := &fallbackTestAgent{
		name: "pi",
		run:  func() (*Result, error) { return nil, terminal },
	}

	_, err := NewFallback([]Agent{quota, generic}).Run(context.Background(), RunOpts{})
	if !errors.Is(err, terminal) {
		t.Fatalf("Run() error = %v, want terminal generic error", err)
	}
	var exhausted *quotaFallbackError
	if errors.As(err, &exhausted) {
		t.Fatalf("Run() error = %v, must not replace generic error with exhaustion summary", err)
	}
	if !HadQuotaFallback(err) {
		t.Fatalf("Run() error = %v, want quota-transition signal", err)
	}
}

func TestClassifyProviderErrorRequiresExhaustionEvidence(t *testing.T) {
	generic := errors.New("provider failed")
	for _, diagnostic := range []string{
		"unknown option rate_limit",
		"configuration field: rate limit",
		"quota limit setting is invalid",
	} {
		if got := ClassifyProviderError(generic, diagnostic); got != generic {
			t.Fatalf("diagnostic %q classified as quota exhaustion: %v", diagnostic, got)
		}
	}
	for _, diagnostic := range []string{
		"rate_limit_error",
		"session_limit_error",
		"quota_exceeded",
		"rate limit exceeded",
		"HTTP 429 Too Many Requests",
		"request failed with 429: provider unavailable",
	} {
		if got := ClassifyProviderError(generic, diagnostic); got == generic {
			t.Fatalf("diagnostic %q was not classified", diagnostic)
		}
	}
	for _, diagnostic := range []string{
		"request failed with 1429",
		"processed 429 tokens",
		"provider error 429",
	} {
		if got := ClassifyProviderError(generic, diagnostic); got != generic {
			t.Fatalf("diagnostic %q classified without a bounded status signal: %v", diagnostic, got)
		}
	}
}

func TestFallbackAgentDoesNotFallbackForGenericFailureOrSilence(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "generic", err: errors.New("provider failed")},
		{name: "silent exit without quota evidence", err: errors.New("claude returned no result event")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &fallbackTestAgent{name: "claude", run: func() (*Result, error) { return nil, tt.err }}
			second := &fallbackTestAgent{name: "pi", run: func() (*Result, error) { return &Result{Text: "must not run"}, nil }}
			_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			if first.calls != 1 || second.calls != 0 {
				t.Fatalf("calls = first %d second %d, want 1/0", first.calls, second.calls)
			}
		})
	}
}

func TestFallbackAgentNeverSwitchesWhileInvocationIsActive(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	secondStarted := make(chan struct{})
	first := &fallbackTestAgent{
		name: "claude",
		runWithOpts: func(RunOpts) (*Result, error) {
			close(started)
			<-release
			err := errors.New("claude exited: exit status 1: rate limit exceeded")
			return nil, ClassifyProviderError(err, "rate limit exceeded")
		},
	}
	second := &fallbackTestAgent{
		name: "pi",
		runWithOpts: func(RunOpts) (*Result, error) {
			close(secondStarted)
			return &Result{Text: "ok"}, nil
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
		done <- err
	}()
	<-started
	select {
	case <-secondStarted:
		t.Fatal("fallback agent started before the active invocation ended")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback did not complete")
	}
	select {
	case <-secondStarted:
	default:
		t.Fatal("fallback agent never started after the first invocation ended")
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

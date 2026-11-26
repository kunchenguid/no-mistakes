package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// quotaTestAgent is a fallback chain member that records the session it was
// handed, so a test can prove a switch continued with a fresh session.
type quotaTestAgent struct {
	name       string
	run        func(opts RunOpts) (*Result, error)
	calls      int
	sessions   []*SessionRef
	resumable  bool
	providerOf string
}

func (a *quotaTestAgent) Name() string { return a.name }

func (a *quotaTestAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	a.calls++
	if opts.Session != nil {
		a.sessions = append(a.sessions, opts.Session)
	}
	return a.run(opts)
}

func (a *quotaTestAgent) Close() error { return nil }

func (a *quotaTestAgent) SupportsSessionResume() bool { return a.resumable }

func (a *quotaTestAgent) SupportsSessionProvider(provider string) bool {
	return a.providerOf == provider
}

func quotaError(who string) error {
	return errors.New(who + " failed: quota exceeded")
}

func TestFallbackQuotaSwitchMovesToTheNextAgentOnAQuotaError(t *testing.T) {
	claude := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) {
		return nil, quotaError("claude")
	}}
	codex := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	var events []LifecycleEvent
	result, err := NewFallback([]Agent{claude, codex}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{
		OnLifecycle: func(event LifecycleEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want codex's ok", result)
	}
	if claude.calls != 1 || codex.calls != 1 {
		t.Fatalf("calls = claude %d codex %d, want 1/1: each agent gets exactly one chance", claude.calls, codex.calls)
	}
	switches := 0
	for _, event := range events {
		if event.Phase != LifecyclePhaseQuotaSwitch {
			continue
		}
		switches++
		if event.Agent != "claude" || !strings.Contains(event.Message, "codex") {
			t.Fatalf("quota switch event = %+v, want claude -> codex", event)
		}
		// The executor's switch recorder keys on the structured field, never
		// on parsing the prose.
		if event.NextAgent != "codex" {
			t.Fatalf("quota switch event NextAgent = %q, want codex", event.NextAgent)
		}
		if !strings.Contains(event.Message, "fresh session") {
			t.Fatalf("quota switch event = %+v, want it to name the fresh session", event)
		}
	}
	if switches != 1 {
		t.Fatalf("quota switch events = %d, want 1", switches)
	}
}

func TestFallbackQuotaSwitchWalksTheWholeOrderAndNeverSwitchesBack(t *testing.T) {
	claude := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) {
		return nil, quotaError("claude")
	}}
	codex := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) {
		return nil, errors.New(`codex failed: insufficient_quota`)
	}}
	grok := &quotaTestAgent{name: "grok", run: func(RunOpts) (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	result, err := NewFallback([]Agent{claude, codex, grok}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want grok's ok", result)
	}
	if claude.calls != 1 || codex.calls != 1 || grok.calls != 1 {
		t.Fatalf("calls = claude %d codex %d grok %d, want 1/1/1", claude.calls, codex.calls, grok.calls)
	}
}

func TestFallbackQuotaSwitchContinuesWithAFreshSession(t *testing.T) {
	// A claude session exists (this invocation resumed one) and claude hits a
	// quota error: codex must run with NO session, never claude's.
	claude := &quotaTestAgent{name: "claude", resumable: true, providerOf: "claude", run: func(RunOpts) (*Result, error) {
		return nil, quotaError("claude")
	}}
	codex := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	result, err := NewFallback([]Agent{claude, codex}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{
		Session: &SessionRef{Agent: "claude", ID: "sess-1"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want codex's ok", result)
	}
	if claude.calls != 1 || codex.calls != 1 {
		t.Fatalf("calls = claude %d codex %d, want 1/1", claude.calls, codex.calls)
	}
	if len(codex.sessions) != 0 {
		t.Fatalf("codex received %+v, want no session at all: the switch continues fresh", codex.sessions)
	}
}

func TestFallbackQuotaSwitchDoesNotFireOnOtherErrors(t *testing.T) {
	parseErr := errors.New(`codex output parse: invalid JSON (output snippet: "not json")`)
	claude := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) {
		return nil, quotaError("claude")
	}}
	codex := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) {
		return nil, parseErr
	}}
	_, err := NewFallback([]Agent{claude, codex}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{})
	if !errors.Is(err, parseErr) {
		t.Fatalf("Run() error = %v, want %v: a non-quota terminal error never switches", err, parseErr)
	}
	if codex.calls != 1 {
		t.Fatalf("codex calls = %d, want 1", codex.calls)
	}
}

func TestFallbackQuotaSwitchKeepsTheUnavailableFallbackAsItWas(t *testing.T) {
	claude := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) {
		return nil, errors.New(`claude start: exec: "claude": executable file not found`)
	}}
	codex := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	result, err := NewFallback([]Agent{claude, codex}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want the unchanged unavailable fallback", result)
	}
}

func TestFallbackWithoutQuotaSwitchNeverMovesOnAQuotaError(t *testing.T) {
	claude := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) {
		return nil, quotaError("claude")
	}}
	codex := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) {
		return &Result{Text: "should not run"}, nil
	}}
	_, err := NewFallback([]Agent{claude, codex}).Run(context.Background(), RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("Run() error = %v, want the quota error", err)
	}
	if codex.calls != 0 {
		t.Fatalf("codex calls = %d, want 0: the switch exists only when quota routing is active", codex.calls)
	}
}

func exitStatusOneError(t *testing.T) *exec.ExitError {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", "1")
	} else {
		cmd = exec.Command("sh", "-c", "exit 1")
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	return exitErr
}

// codexUsageLimitFixtures holds verbatim messages from openai/codex
// (codex-rs/protocol/src/error.rs UsageLimitReachedError) plus the issue
// #1806 log wording.
var codexUsageLimitFixtures = []struct {
	name   string
	detail string
}{
	{
		name:   "plus usage limit curly apostrophe",
		detail: "You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 8:37 PM.",
	},
	{
		name:   "plus usage limit ascii apostrophe",
		detail: "You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/settings/usage to purchase more credits or try again later.",
	},
	{
		name:   "model-specific usage limit",
		detail: "You’ve hit your usage limit for gpt-5.3-codex. Switch to another model now, or try again later.",
	},
	{
		name:   "workspace owner credits depleted",
		detail: "Your workspace is out of credits. Add credits to continue.",
	},
	{
		name:   "workspace member credits depleted",
		detail: "Your workspace is out of credits. Ask your workspace owner to refill in order to continue.",
	},
	{
		name:   "workspace owner spend cap",
		detail: "You hit your spend cap set in your workspace. Increase your spend cap to continue.",
	},
	{
		name:   "workspace member spend cap",
		detail: "You hit your spend cap set by the owner of your workspace. Ask an owner to increase your spend cap to continue.",
	},
}

func TestFallbackQuotaSwitchRecognizesEveryExistingQuotaShape(t *testing.T) {
	exitErr := exitStatusOneError(t)
	cases := map[string]error{
		"free usage limit":         errors.New("claude failed: You have hit your free usage limit"),
		"insufficient quota":       errors.New("codex failed: insufficient quota"),
		"exceeded current quota":   errors.New("codex failed: You exceeded your current quota, please check your plan and billing details"),
		"quota exhausted":          errors.New("grok failed: quota exhausted"),
		"claude usage limit event": &claudeAPIError{Status: 429, usageLimitReached: true, Message: "rate limited"},
		"codex quota exceeded":     fmt.Errorf("codex exited: %w: %s", exitErr, "Quota exceeded. Check your plan and billing details."),
	}
	for _, fixture := range codexUsageLimitFixtures {
		cases["codex "+fixture.name] = fmt.Errorf("codex exited: %w: %s", exitErr, fixture.detail)
	}
	for name, quotaErr := range cases {
		t.Run(name, func(t *testing.T) {
			if !IsQuotaExhaustionError(quotaErr) {
				t.Fatalf("IsQuotaExhaustionError(%v) = false, want true", quotaErr)
			}
		})
	}
}

func TestIsQuotaExhaustionError_RejectsNonQuotaErrors(t *testing.T) {
	exitErr := exitStatusOneError(t)
	for name, err := range map[string]error{
		"nil":                                   nil,
		"context":                               context.Canceled,
		"transient":                             errors.New("claude failed: overloaded_error"),
		"unavailable":                           errors.New(`claude start: executable file not found`),
		"structured parse":                      errors.New(`codex output parse: invalid JSON`),
		"codex ordinary exit":                   fmt.Errorf("codex exited: %w: %s", exitErr, "stream disconnected before completion"),
		"codex approaching usage limit warning": fmt.Errorf("codex exited: %w: %s", exitErr, "Warning: approaching your usage limit"),
		"codex approaching spend cap warning":   fmt.Errorf("codex exited: %w: %s", exitErr, "Warning: approaching your spend cap"),
		"codex spend limit config error":        fmt.Errorf("codex exited: %w: %s", exitErr, "invalid spend limit configuration"),
		"codex unrelated credits error":         fmt.Errorf("codex exited: %w: %s", exitErr, "workspace credits report could not be parsed"),
	} {
		t.Run(name, func(t *testing.T) {
			if IsQuotaExhaustionError(err) {
				t.Fatalf("IsQuotaExhaustionError(%v) = true, want false", err)
			}
		})
	}
}

func TestReviewerQuotaFallbackSwitchesOnCodexUsageLimitWordings(t *testing.T) {
	exitErr := exitStatusOneError(t)
	for _, tc := range codexUsageLimitFixtures {
		t.Run(tc.name, func(t *testing.T) {
			codexErr := fmt.Errorf("codex exited: %w: %s", exitErr, tc.detail)
			primary := &quotaTestAgent{name: "primary"}
			codex := &quotaTestAgent{
				name:       "codex",
				resumable:  true,
				providerOf: "codex",
				run: func(RunOpts) (*Result, error) {
					return nil, codexErr
				},
			}
			claude := &quotaTestAgent{
				name:       "claude",
				resumable:  true,
				providerOf: "claude",
				run: func(RunOpts) (*Result, error) {
					return &Result{Text: "ok"}, nil
				},
			}
			ag := WithReviewRoles(primary, ReviewRoles{
				Reviewer: RoundedRole{
					Agent: NewFallback([]Agent{codex, claude}, WithReviewerQuotaFallback("", "")),
				},
			})
			var events []LifecycleEvent
			result, err := ag.Run(context.Background(), RunOpts{
				Purpose:     "review",
				Session:     &SessionRef{Agent: "codex", ID: "stale-codex-session"},
				OnLifecycle: func(event LifecycleEvent) { events = append(events, event) },
			})
			if err != nil {
				t.Fatalf("Run() error = %v, want fallback to claude", err)
			}
			if result == nil || result.Text != "ok" || result.Provider != "claude" {
				t.Fatalf("Run() result = %+v, want claude's ok", result)
			}
			if primary.calls != 0 || codex.calls != 1 || claude.calls != 1 {
				t.Fatalf("calls = primary %d codex %d claude %d, want 0/1/1", primary.calls, codex.calls, claude.calls)
			}
			if len(claude.sessions) != 0 {
				t.Fatalf("claude sessions = %+v, want a fresh session (nil)", claude.sessions)
			}
			if len(events) != 1 || events[0].Phase != LifecyclePhaseFallback || events[0].Agent != "codex" || !strings.Contains(events[0].Message, "claude") {
				t.Fatalf("events = %+v, want one fallback event from codex to claude", events)
			}
			if label, retry := classifyTransient(fmt.Errorf("codex exited: %w: HTTP 429: %s", exitErr, tc.detail)); retry {
				t.Fatalf("classifyTransient() = (%q, true), want terminal non-retryable for Codex usage limit", label)
			}
		})
	}
}

func TestReviewerQuotaFallbackDoesNotSwitchOnOrdinaryCodexErrors(t *testing.T) {
	exitErr := exitStatusOneError(t)
	for _, tc := range []struct {
		name   string
		detail string
	}{
		{name: "stream disconnected", detail: "stream disconnected before completion"},
		{name: "approaching usage limit warning", detail: "Warning: approaching your usage limit"},
		{name: "approaching spend cap warning", detail: "Warning: approaching your spend cap"},
		{name: "spend limit config error", detail: "invalid spend limit configuration"},
		{name: "unrelated credits error", detail: "workspace credits report could not be parsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexErr := fmt.Errorf("codex exited: %w: %s", exitErr, tc.detail)
			primary := &quotaTestAgent{name: "primary"}
			codex := &quotaTestAgent{
				name: "codex",
				run: func(RunOpts) (*Result, error) {
					return nil, codexErr
				},
			}
			claude := &quotaTestAgent{
				name: "claude",
				run: func(RunOpts) (*Result, error) {
					return &Result{Text: "unexpected"}, nil
				},
			}
			ag := WithReviewRoles(primary, ReviewRoles{
				Reviewer: RoundedRole{
					Agent: NewFallback([]Agent{codex, claude}, WithReviewerQuotaFallback("", "")),
				},
			})
			var events []LifecycleEvent
			_, err := ag.Run(context.Background(), RunOpts{
				Purpose:     "review",
				OnLifecycle: func(event LifecycleEvent) { events = append(events, event) },
			})
			if !errors.Is(err, codexErr) {
				t.Fatalf("Run() error = %v, want original codex error %v", err, codexErr)
			}
			if primary.calls != 0 || codex.calls != 1 || claude.calls != 0 {
				t.Fatalf("calls = primary %d codex %d claude %d, want 0/1/0", primary.calls, codex.calls, claude.calls)
			}
			if len(events) != 0 {
				t.Fatalf("events = %+v, want no fallback events", events)
			}
		})
	}
}

// A plain nil QuotaSwitch option keeps NewFallback's signature friendly to
// existing callers and mirrors the pipeline's agent name type at the seam.
var _ = types.AgentName("")

func TestFallbackQuotaSwitchPersistsAcrossTurns(t *testing.T) {
	first := &quotaTestAgent{name: "claude", resumable: true, providerOf: "claude", run: func(RunOpts) (*Result, error) {
		return nil, errors.New("claude exited: quota exceeded")
	}}
	second := &quotaTestAgent{name: "codex", resumable: true, providerOf: "codex", run: func(RunOpts) (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	chain := NewFallback([]Agent{first, second}, WithQuotaSwitch(""))
	for _, session := range []*SessionRef{{Agent: "claude", ID: "old"}, {Agent: "codex", ID: "new"}, {Agent: "claude", ID: "stale"}} {
		if _, err := chain.Run(context.Background(), RunOpts{Session: session}); err != nil {
			t.Fatal(err)
		}
	}
	if first.calls != 1 || second.calls != 3 || chain.Name() != "codex" {
		t.Fatalf("calls = %d/%d, active = %s", first.calls, second.calls, chain.Name())
	}
	if len(second.sessions) != 3 || second.sessions[0].ID != "" || second.sessions[1].ID != "new" || second.sessions[2].ID != "" {
		t.Fatalf("sessions = %+v, want fresh/resumed/fresh codex sessions", second.sessions)
	}
}

func TestFallbackQuotaSwitchRestoresPositionWithAFreshSession(t *testing.T) {
	first := &quotaTestAgent{name: "claude", resumable: true, providerOf: "claude"}
	second := &quotaTestAgent{name: "codex", resumable: true, providerOf: "codex", run: func(RunOpts) (*Result, error) {
		return &Result{Text: "ok"}, nil
	}}
	chain := NewFallback([]Agent{first, second}, WithQuotaSwitch("codex"))
	for i := 0; i < 2; i++ {
		if _, err := chain.Run(context.Background(), RunOpts{Session: &SessionRef{Agent: "codex", ID: "session"}}); err != nil {
			t.Fatal(err)
		}
	}
	if first.calls != 0 || second.calls != 2 || len(second.sessions) != 2 || second.sessions[0].ID != "" || second.sessions[1].ID != "session" {
		t.Fatalf("calls = %d/%d, resumed sessions = %d", first.calls, second.calls, len(second.sessions))
	}
}

func TestFallbackNativeQuotaErrorsEmitQuotaSwitchAndDropTheNextProvidersSession(t *testing.T) {
	for _, name := range []string{"claude", "codex", "pi"} {
		t.Run(name, func(t *testing.T) {
			first := &quotaTestAgent{name: name, run: func(RunOpts) (*Result, error) {
				return nil, errors.New(name + " exited: quota exceeded")
			}}
			second := &quotaTestAgent{name: "other", resumable: true, providerOf: "other", run: func(RunOpts) (*Result, error) {
				return &Result{Text: "ok"}, nil
			}}
			var switches, fallbacks int
			_, err := NewFallback([]Agent{first, second}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{
				Session: &SessionRef{Agent: "other", ID: "old-other-session"},
				OnLifecycle: func(event LifecycleEvent) {
					if event.Phase == LifecyclePhaseQuotaSwitch {
						switches++
					}
					if event.Phase == LifecyclePhaseFallback {
						fallbacks++
					}
				},
			})
			if err != nil || switches != 1 || fallbacks != 0 || len(second.sessions) != 1 || second.sessions[0].ID != "" || second.sessions[0].Agent != "" {
				t.Fatalf("error=%v switches=%d fallbacks=%d sessions=%v", err, switches, fallbacks, second.sessions)
			}
		})
	}
}

func TestFallbackQuotaSwitchAdvancesAgainOnALaterTurn(t *testing.T) {
	first := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) { return nil, quotaError("claude") }}
	calls := 0
	second := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) {
		calls++
		if calls == 1 {
			return &Result{Text: "ok"}, nil
		}
		return nil, quotaError("codex")
	}}
	third := &quotaTestAgent{name: "pi", run: func(RunOpts) (*Result, error) { return &Result{Text: "ok"}, nil }}
	chain := NewFallback([]Agent{first, second, third}, WithQuotaSwitch(""))
	var transitions []string
	for i := 0; i < 3; i++ {
		if _, err := chain.Run(context.Background(), RunOpts{OnLifecycle: func(event LifecycleEvent) {
			if event.Phase == LifecyclePhaseQuotaSwitch {
				transitions = append(transitions, event.Agent+":"+event.NextAgent)
			}
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if first.calls != 1 || second.calls != 2 || third.calls != 2 || strings.Join(transitions, ",") != "claude:codex,codex:pi" {
		t.Fatalf("calls=%d/%d/%d transitions=%v", first.calls, second.calls, third.calls, transitions)
	}
}

func TestFallbackQuotaSwitchDoesNotConsumeStructuredRejection(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprint(wrapped), func(t *testing.T) {
			_, rejection := finalizeTextResult("pi", `{"ok":"claude exited: quota exceeded"}`, schema, TokenUsage{})
			if !IsStructuredOutputRejected(rejection) {
				t.Fatalf("expected schema rejection, got %v", rejection)
			}
			if wrapped {
				rejection = fmt.Errorf("validation: %w", rejection)
			}
			if IsQuotaExhaustionError(rejection) {
				t.Fatalf("schema rejection is not a quota error: %v", rejection)
			}
			first := &quotaTestAgent{name: "pi", run: func(RunOpts) (*Result, error) { return nil, rejection }}
			second := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) { return &Result{Text: "unexpected"}, nil }}
			chain := NewFallback([]Agent{first, second}, WithQuotaSwitch(""))
			switches := 0
			for i := 0; i < 2; i++ {
				_, err := chain.Run(context.Background(), RunOpts{OnLifecycle: func(event LifecycleEvent) {
					if event.Phase == LifecyclePhaseQuotaSwitch {
						switches++
					}
				}})
				if !errors.Is(err, rejection) {
					t.Fatalf("error=%v, want schema rejection", err)
				}
			}
			if first.calls != 2 || second.calls != 0 || switches != 0 || chain.Name() != "pi" {
				t.Fatalf("calls=%d/%d switches=%d head=%s", first.calls, second.calls, switches, chain.Name())
			}
		})
	}
}

func TestRestoredQuotaSwitchStartsADurableSessionWithOneRemainingAgent(t *testing.T) {
	remaining := &quotaTestAgent{name: "pi", resumable: true, providerOf: "pi", run: func(opts RunOpts) (*Result, error) { return &Result{Text: "ok"}, nil }}
	chain := NewFallback([]Agent{remaining}, WithQuotaSwitch("pi"))
	for i := 0; i < 2; i++ {
		if _, err := chain.Run(context.Background(), RunOpts{Session: &SessionRef{Agent: "pi", ID: "previous-session"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(remaining.sessions) != 2 || remaining.sessions[0].ID != "" || remaining.sessions[1].ID != "previous-session" {
		t.Fatalf("restored singleton must start durably before resuming: %+v", remaining.sessions)
	}
}

func TestQuotaSwitchDoesNotCreateUnrequestedSessions(t *testing.T) {
	first := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) { return nil, quotaError("claude") }}
	second := &quotaTestAgent{name: "pi", resumable: true, providerOf: "pi", run: func(RunOpts) (*Result, error) { return &Result{Text: "ok"}, nil }}
	if _, err := NewFallback([]Agent{first, second}, WithQuotaSwitch("")).Run(context.Background(), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(second.sessions) != 0 {
		t.Fatalf("cold invocation acquired a durable session request: %+v", second.sessions)
	}
}

func TestQuotaFallbackRestoresEitherACPAliasSpelling(t *testing.T) {
	for _, tc := range []struct{ start, adapter string }{
		{"cursor", "acp:cursor"}, {"acp:cursor", "cursor"},
		{"devin", "acp:devin"}, {"acp:devin", "devin"},
	} {
		first := &quotaTestAgent{name: "claude", run: func(RunOpts) (*Result, error) { return nil, errors.New("exhausted agent revisited") }}
		next := &quotaTestAgent{name: tc.adapter, resumable: true, providerOf: tc.adapter, run: func(RunOpts) (*Result, error) { return &Result{Text: "ok"}, nil }}
		chain := NewFallback([]Agent{first, next}, WithQuotaSwitch(tc.start))
		for _, id := range []string{"old-session", "new-session"} {
			result, err := chain.Run(context.Background(), RunOpts{Session: &SessionRef{Agent: tc.adapter, ID: id}})
			if err != nil || result == nil || result.Provider != tc.adapter {
				t.Fatalf("start=%s adapter=%s result=%+v error=%v", tc.start, tc.adapter, result, err)
			}
		}
		if first.calls != 0 || next.calls != 2 || len(next.sessions) != 2 || next.sessions[0].ID != "" || next.sessions[1].ID != "new-session" {
			t.Fatalf("start=%s adapter=%s calls=%d/%d sessions=%+v", tc.start, tc.adapter, first.calls, next.calls, next.sessions)
		}
	}
}

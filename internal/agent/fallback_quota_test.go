package agent

import (
	"context"
	"errors"
	"fmt"
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

func TestFallbackQuotaSwitchRecognizesEveryExistingQuotaShape(t *testing.T) {
	for name, quotaErr := range map[string]error{
		"free usage limit":         errors.New("claude failed: You have hit your free usage limit"),
		"insufficient quota":       errors.New("codex failed: insufficient quota"),
		"exceeded current quota":   errors.New("codex failed: You exceeded your current quota, please check your plan and billing details"),
		"quota exhausted":          errors.New("grok failed: quota exhausted"),
		"claude usage limit event": &claudeAPIError{Status: 429, usageLimitReached: true, Message: "rate limited"},
	} {
		t.Run(name, func(t *testing.T) {
			if !IsQuotaExhaustionError(quotaErr) {
				t.Fatalf("IsQuotaExhaustionError(%v) = false, want true", quotaErr)
			}
		})
	}
}

func TestIsQuotaExhaustionError_RejectsNonQuotaErrors(t *testing.T) {
	for name, err := range map[string]error{
		"nil":              nil,
		"context":          context.Canceled,
		"transient":        errors.New("claude failed: overloaded_error"),
		"unavailable":      errors.New(`claude start: executable file not found`),
		"structured parse": errors.New(`codex output parse: invalid JSON`),
	} {
		t.Run(name, func(t *testing.T) {
			if IsQuotaExhaustionError(err) {
				t.Fatalf("IsQuotaExhaustionError(%v) = true, want false", err)
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

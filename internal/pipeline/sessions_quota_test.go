package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestQuotaSwitchPreservesDurableSessionRequests(t *testing.T) {
	for _, mode := range []string{"quota", "provider-mismatch", "restored"} {
		t.Run(mode, func(t *testing.T) {
			database, run := sessionTestDB(t)
			claude := newFakeSessionAgent()
			claude.name = "claude"
			pi := newFakeSessionAgent()
			pi.name = "pi"
			pi.nextID = 10
			oldProvider := "claude"
			candidates := []agent.Agent{claude, pi}
			start := ""
			switch mode {
			case "quota":
				claude.failNext = errors.New("claude exited: quota exceeded")
			case "provider-mismatch":
				candidates = []agent.Agent{pi, claude}
			case "restored":
				start = "pi"
				oldProvider = "pi"
			}
			chain := agent.NewFallback(candidates, agent.WithQuotaSwitch(start))
			if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), oldProvider, "old-session"); err != nil {
				t.Fatal(err)
			}
			sessions := NewRunSessions(database, run.ID, chain, true)
			for i := 0; i < 3; i++ {
				if i == 2 {
					sessions = NewRunSessions(database, run.ID, chain, true)
				}
				result, err := sessions.Run(context.Background(), chain, SessionRoleFixer, agent.RunOpts{Prompt: "fix"}, nil)
				if err != nil || result == nil || result.SessionID != "sess-11" || result.Provider != "pi" || result.Resumed != (i > 0) {
					t.Fatalf("turn=%d result=%+v error=%v", i, result, err)
				}
			}
			if len(pi.calls) != 3 || pi.calls[0].session == nil || pi.calls[0].session.ID != "" || pi.calls[0].session.Agent != "" || pi.calls[1].session.ID != "sess-11" {
				t.Fatalf("Pi must start durably, then resume: %+v", pi.calls)
			}
			wantClaudeCalls := 0
			if mode == "quota" {
				wantClaudeCalls = 1
			}
			if len(claude.calls) != wantClaudeCalls {
				t.Fatalf("claude calls=%d want=%d", len(claude.calls), wantClaudeCalls)
			}
			stored, err := database.GetRunAgentSessions(run.ID)
			if err != nil || len(stored) != 1 || stored[0].Agent != "pi" || stored[0].SessionID != "sess-11" {
				t.Fatalf("durable provider was not replaced: %+v error=%v", stored, err)
			}
		})
	}
}

func TestQuotaFreshSessionFailuresNeverTriggerResumeRecovery(t *testing.T) {
	for _, mode := range []string{"quota", "provider-mismatch", "restored"} {
		for _, tc := range []struct {
			failure  error
			advances bool
		}{
			{errors.New("codex exited: quota exceeded"), true},
			{errors.New("fresh turn failed"), false},
		} {
			failure := tc.failure
			database, run := sessionTestDB(t)
			claude := newFakeSessionAgent()
			claude.name = "claude"
			codex := newFakeSessionAgent()
			codex.name = "codex"
			codex.failNext = failure
			oldProvider := "claude"
			start := ""
			candidates := []agent.Agent{claude, codex}
			switch mode {
			case "quota":
				claude.failNext = errors.New("claude exited: quota exceeded")
			case "provider-mismatch":
				candidates = []agent.Agent{codex, claude}
				claude.failNext = failure
			case "restored":
				start = "codex"
				oldProvider = "codex"
			}
			chain := agent.NewFallback(candidates, agent.WithQuotaSwitch(start))
			if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), oldProvider, "saved-session"); err != nil {
				t.Fatal(err)
			}
			sessions := NewRunSessions(database, run.ID, chain, true)
			_, err := sessions.Run(context.Background(), chain, SessionRoleFixer, agent.RunOpts{Prompt: "fix"}, nil)
			if !errors.Is(err, failure) || len(codex.calls) != 1 || codex.calls[0].session == nil || codex.calls[0].session.ID != "" {
				t.Fatalf("mode=%s failure=%v error=%v calls=%+v", mode, failure, err, codex.calls)
			}
			wantClaudeCalls := 0
			if mode == "quota" || (mode == "provider-mismatch" && tc.advances) {
				wantClaudeCalls = 1
			}
			if len(claude.calls) != wantClaudeCalls {
				t.Fatalf("mode=%s claude calls=%d", mode, len(claude.calls))
			}
			if mode == "provider-mismatch" && tc.advances && (claude.calls[0].session == nil || claude.calls[0].session.ID != "") {
				t.Fatalf("fresh fallback reused a saved session: %+v", claude.calls)
			}
		}
	}
}

func TestQuotaFallbackRetainsRecoveryForAnActualResumeFailure(t *testing.T) {
	database, run := sessionTestDB(t)
	claude := newFakeSessionAgent()
	claude.name = "claude"
	claude.failNext = errors.New("session not found")
	codex := newFakeSessionAgent()
	codex.name = "codex"
	chain := agent.NewFallback([]agent.Agent{claude, codex}, agent.WithQuotaSwitch(""))
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "claude", "saved-session"); err != nil {
		t.Fatal(err)
	}
	sessions := NewRunSessions(database, run.ID, chain, true)
	result, err := sessions.Run(context.Background(), chain, SessionRoleFixer, agent.RunOpts{Prompt: "fix"}, nil)
	if err != nil || result.SessionID != "sess-1" || len(claude.calls) != 2 || len(codex.calls) != 0 || claude.calls[0].session.ID != "saved-session" || claude.calls[1].session.ID != "" || !claude.calls[1].fallback {
		t.Fatalf("result=%+v error=%v claude=%+v codex=%+v", result, err, claude.calls, codex.calls)
	}
}

func TestQuotaExhaustionOnResumedSuccessorDoesNotRetryEnabledChain(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		enabled, predecessor bool
		claudeCalls          int
		fails                bool
	}{
		{"enabled successor", true, true, 1, true},
		{"enabled single agent", true, false, 0, true},
		{"disabled successor", false, true, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, run := sessionTestDB(t)
			claude := newFakeSessionAgent()
			claude.name = "claude"
			claude.failNext = errors.New("claude exited: quota exceeded")
			codex := newFakeSessionAgent()
			codex.name = "codex"
			candidates := []agent.Agent{codex}
			if tc.predecessor {
				candidates = []agent.Agent{claude, codex}
			}
			var options []agent.FallbackOption
			if tc.enabled {
				options = append(options, agent.WithQuotaSwitch(""))
			}
			chain := agent.NewFallback(candidates, options...)
			sessions := NewRunSessions(database, run.ID, chain, true)
			first, err := sessions.Run(context.Background(), chain, SessionRoleFixer, agent.RunOpts{Prompt: "fix"}, nil)
			if err != nil || first == nil || first.Provider != "codex" {
				t.Fatalf("first result=%+v error=%v", first, err)
			}
			exhausted := errors.New("codex exited: quota exceeded")
			codex.failNext = exhausted
			result, err := sessions.Run(context.Background(), chain, SessionRoleFixer, agent.RunOpts{Prompt: "fix again"}, nil)
			if tc.fails {
				if !errors.Is(err, exhausted) {
					t.Fatalf("error=%v want quota exhaustion", err)
				}
			} else if err != nil || result == nil || result.Provider != "claude" || result.SessionID == "" {
				t.Fatalf("disabled routing must retain cold retry: result=%+v error=%v", result, err)
			}
			if len(codex.calls) != 2 || codex.calls[1].session == nil || codex.calls[1].session.ID != first.SessionID {
				t.Fatalf("Codex calls=%+v want %d attempts with a resumed second turn", codex.calls, 2)
			}
			if len(claude.calls) != tc.claudeCalls {
				t.Fatalf("Claude attempts=%d want=%d", len(claude.calls), tc.claudeCalls)
			}
			if !tc.enabled && (claude.calls[1].session == nil || claude.calls[1].session.ID != "" || !claude.calls[1].fallback) {
				t.Fatalf("disabled routing must restart the configured chain with a fresh session: %+v", claude.calls)
			}
		})
	}
}

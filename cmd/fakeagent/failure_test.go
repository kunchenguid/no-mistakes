package main

import (
	"path/filepath"
	"testing"
)

// TestMatchBranchesOnModel proves one scenario returns different behavior per
// routed Candidate model, which prompt matching alone cannot express.
func TestMatchBranchesOnModel(t *testing.T) {
	s := &Scenario{Actions: []Action{
		{Match: "Fix", Model: "sol", Structured: map[string]any{"which": "sol"}},
		{Match: "Fix", Model: "luna", Structured: map[string]any{"which": "luna"}},
		{Text: "catch-all"},
	}}
	if got := s.Match("Fix the finding", "gpt-5.6-luna", "medium"); got.Structured["which"] != "luna" {
		t.Fatalf("luna candidate matched %v, want the luna action", got.Structured)
	}
	if got := s.Match("Fix the finding", "gpt-5.6-sol", "xhigh"); got.Structured["which"] != "sol" {
		t.Fatalf("sol candidate matched %v, want the sol action", got.Structured)
	}
	if got := s.Match("unrelated prompt", "gpt-5.6-luna", "low"); got.Text != "catch-all" {
		t.Fatalf("non-matching prompt = %q, want the catch-all", got.Text)
	}
}

// TestMatchBranchesOnEffort proves a verifier can resolve only at xhigh effort
// (authority_strong), the mechanism that drives cascade escalation.
func TestMatchBranchesOnEffort(t *testing.T) {
	s := &Scenario{Actions: []Action{
		{Match: "verify", Effort: "xhigh", Structured: map[string]any{"verdict": "resolved"}},
		{Match: "verify", Structured: map[string]any{"verdict": "unresolved"}},
	}}
	if got := s.Match("please verify", "gpt-5.6-sol", "high"); got.Structured["verdict"] != "unresolved" {
		t.Fatalf("high-effort verifier = %v, want unresolved", got.Structured)
	}
	if got := s.Match("please verify", "gpt-5.6-sol", "xhigh"); got.Structured["verdict"] != "resolved" {
		t.Fatalf("xhigh-effort verifier = %v, want resolved", got.Structured)
	}
}

// TestMaybeInjectFailureTransient proves the transient counter fails FailTimes
// execs then falls through to success, so an adapter retry can be observed.
func TestMaybeInjectFailureTransient(t *testing.T) {
	t.Setenv("FAKEAGENT_LOG", filepath.Join(t.TempDir(), "fakeagent.log"))
	action := Action{Match: "flaky", Fail: "transient", FailTimes: 2}
	for attempt := 1; attempt <= 2; attempt++ {
		code, handled := maybeInjectFailure("codex", action)
		if !handled || code == 0 {
			t.Fatalf("attempt %d = (code=%d, handled=%v), want a handled non-zero failure", attempt, code, handled)
		}
	}
	if code, handled := maybeInjectFailure("codex", action); handled {
		t.Fatalf("attempt 3 = (code=%d, handled=%v), want fall-through to success", code, handled)
	}
}

// TestMaybeInjectFailureModes covers operational (handled non-zero), the
// no-fail default (not handled), and output (handled at exit 0).
func TestMaybeInjectFailureModes(t *testing.T) {
	if code, handled := maybeInjectFailure("codex", Action{Fail: "operational"}); !handled || code == 0 {
		t.Fatalf("operational = (code=%d, handled=%v), want handled non-zero", code, handled)
	}
	if _, handled := maybeInjectFailure("codex", Action{}); handled {
		t.Fatalf("empty fail mode must not be handled")
	}
	if code, handled := maybeInjectFailure("codex", Action{Fail: "output"}); !handled || code != 0 {
		t.Fatalf("output = (code=%d, handled=%v), want handled exit 0", code, handled)
	}
}

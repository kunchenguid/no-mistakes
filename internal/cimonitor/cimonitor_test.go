package cimonitor

import (
	"strings"
	"testing"
)

func TestParseActivity_Empty(t *testing.T) {
	a := ParseActivity(nil)
	if a.CIFixes != 0 || a.AutoFixing || a.Ready || a.DeclaredNoCI || a.LastEvent != "" {
		t.Errorf("expected zero activity for empty logs, got %+v", a)
	}
}

func TestFromAuthoritative_IgnoresSpoofedReadyLog(t *testing.T) {
	a := FromAuthoritative(false, false, []string{NoChecksPassedMsg})
	if a.Ready || a.DeclaredNoCI {
		t.Fatalf("spoofed ready log created readiness: %+v", a)
	}

	a = FromAuthoritative(true, true, []string{ChecksRunningMsg})
	if !a.Ready || !a.DeclaredNoCI {
		t.Fatalf("authoritative declared no-CI state was lost: %+v", a)
	}
}

func TestParseActivity_CountsFixes(t *testing.T) {
	a := ParseActivity([]string{
		"issues detected: test - auto-fixing (attempt 1/3)...",
		"running agent to fix CI issues...",
		"committed and pushed fixes",
		"issues detected: lint - auto-fixing (attempt 2/3)...",
		"running agent to fix CI issues...",
	})
	if a.CIFixes != 2 {
		t.Errorf("expected 2 CI fixes, got %d", a.CIFixes)
	}
	if !a.AutoFixing {
		t.Error("expected auto-fixing true while a fix is in progress")
	}
	if a.Ready {
		t.Error("expected not ready while fixing")
	}
	if a.DeclaredNoCI {
		t.Error("expected DeclaredNoCI false while fixing")
	}
}

func TestChecksPassed(t *testing.T) {
	tests := []struct {
		name         string
		logs         []string
		wantReady    bool
		wantDeclared bool
	}{
		{
			name: "all checks passed",
			logs: []string{
				"monitoring CI for PR #42 (timeout: 4h)...",
				ChecksPassedMsg,
			},
			wantReady:    true,
			wantDeclared: false,
		},
		{
			name:         "declared no_ci with zero checks",
			logs:         []string{NoChecksPassedMsg},
			wantReady:    true,
			wantDeclared: true,
		},
		{
			name: "legacy generic empty-checks marker is never ready",
			// Pre-fix wording that treated unproven empty forge results as
			// green. Must stay not-ready so a PR 607-style log cannot green.
			logs:         []string{"no CI checks reported - still monitoring until merged or closed"},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name: "waiting for delayed registration is never ready",
			logs: []string{
				"monitoring CI for PR #42 (timeout: 4h)...",
				"no CI checks reported yet, waiting for checks to register...",
			},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "still monitoring before checks pass",
			logs:         []string{"monitoring CI for PR #42 (timeout: 4h)..."},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "cleared when checks re-run",
			logs:         []string{ChecksPassedMsg, ChecksRunningMsg},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "declared no_ci cleared when unexpected checks appear",
			logs:         []string{NoChecksPassedMsg, ChecksRunningMsg},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "cleared when a new failure appears",
			logs:         []string{ChecksPassedMsg, "issues detected: test - auto-fixing (attempt 1/3)..."},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "cleared when mergeability pending",
			logs:         []string{ChecksPassedMsg, "mergeable state still pending: unknown"},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "cleared when PR merged",
			logs:         []string{ChecksPassedMsg, "PR has been merged!"},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "ignores unrelated agent output",
			logs:         []string{"CI failures detected: test failed", "agent says this is not ready to merge yet"},
			wantReady:    false,
			wantDeclared: false,
		},
		{
			name:         "empty",
			logs:         nil,
			wantReady:    false,
			wantDeclared: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ChecksPassed(tt.logs); got != tt.wantReady {
				t.Errorf("ChecksPassed() = %v, want %v", got, tt.wantReady)
			}
			if got := DeclaredNoCI(tt.logs); got != tt.wantDeclared {
				t.Errorf("DeclaredNoCI() = %v, want %v", got, tt.wantDeclared)
			}
		})
	}
}

// TestChecksPassed_PR607RealLogSequence replays the exact CI log prefix that
// caused the false checks-passed signal on PR 607. The generic empty-checks
// marker must never make Ready true; the proven all-green PR 605 shape must
// still make Ready true.
func TestChecksPassed_PR607RealLogSequence(t *testing.T) {
	// Verbatim prefix the agent saw when it emitted "checks green" for PR 607
	// (see nm-pr607-merge-safety-diagnostic-r1). Grace had expired; Actions had
	// not yet registered any checks for head b2a0b85.
	pr607Prefix := []string{
		"monitoring CI for PR #607 (timeout: 4h0m0s)...",
		"mergeable state still pending: PENDING",
		"mergeable state still pending: PENDING",
		"no CI checks reported - still monitoring until merged or closed",
	}
	if ChecksPassed(pr607Prefix) {
		t.Fatal("PR 607 empty-checks prefix must not be ready")
	}
	if DeclaredNoCI(pr607Prefix) {
		t.Fatal("PR 607 empty-checks prefix is not a declared no_ci path")
	}

	// Later the same run saw real pending checks - still not ready.
	pr607WithPending := append(append([]string{}, pr607Prefix...), ChecksRunningMsg)
	if ChecksPassed(pr607WithPending) {
		t.Fatal("PR 607 pending checks must not be ready")
	}

	// Proven path (PR 605): all-green marker is ready without DeclaredNoCI.
	pr605Ready := []string{
		"monitoring CI for PR #605 (timeout: 4h0m0s)...",
		ChecksRunningMsg,
		"issues detected: e2e, test (macos-latest), test (ubuntu-latest), test (windows-latest) - auto-fixing (attempt 1/10)...",
		ChecksRunningMsg,
		ChecksPassedMsg,
	}
	if !ChecksPassed(pr605Ready) {
		t.Fatal("PR 605 all-green path must stay ready")
	}
	if DeclaredNoCI(pr605Ready) {
		t.Fatal("PR 605 all-green path must not report DeclaredNoCI")
	}
}

func TestParseActivity_LastEvent(t *testing.T) {
	a := ParseActivity([]string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"PR has been merged!",
	})
	if !strings.Contains(a.LastEvent, "merged") {
		t.Errorf("expected merged as last event, got %q", a.LastEvent)
	}
}

// TestNoChecksConfigured_IsRecognizedAndNeverReady pins the third empty-checks
// state apart from the two that already existed. A determined absence of CI is
// neither a pass nor a declared no_ci pass, and it is not the monitor still
// waiting either: it is a recognized terminal event with no readiness at all.
func TestNoChecksConfigured_IsRecognizedAndNeverReady(t *testing.T) {
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h0m0s)...",
		NoChecksConfiguredMsg,
	}
	if ChecksPassed(logs) {
		t.Fatal("a repository without CI must never report checks passed")
	}
	if DeclaredNoCI(logs) {
		t.Fatal("a determined absence of CI is not the trusted no_ci declaration")
	}
	if got := ParseActivity(logs).LastEvent; got != NoChecksConfiguredMsg {
		t.Fatalf("LastEvent = %q, want the no-CI marker so consumers can render it", got)
	}
	if NoChecksConfiguredMsg == NoChecksPassedMsg {
		t.Fatal("the two empty-checks outcomes must not share one message")
	}
}

// A pass observed earlier in the run must not survive a later determination
// that the repository has no CI, which is what a moved head can produce.
func TestNoChecksConfigured_ClearsAnEarlierReadyState(t *testing.T) {
	logs := []string{ChecksPassedMsg, NoChecksConfiguredMsg}
	if ChecksPassed(logs) {
		t.Fatalf("the no-CI marker must clear an earlier ready state, logs: %v", logs)
	}
}

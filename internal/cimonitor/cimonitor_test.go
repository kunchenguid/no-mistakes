package cimonitor

import (
	"strings"
	"testing"
)

func TestParseActivity_Empty(t *testing.T) {
	a := ParseActivity(nil)
	if a.CIFixes != 0 || a.AutoFixing || a.Ready || a.LastEvent != "" {
		t.Errorf("expected zero activity for empty logs, got %+v", a)
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
}

func TestChecksPassed(t *testing.T) {
	tests := []struct {
		name string
		logs []string
		want bool
	}{
		{
			name: "all checks passed",
			logs: []string{
				"monitoring CI for PR #42 (timeout: 4h)...",
				ChecksPassedMsg,
			},
			want: true,
		},
		{
			name: "no checks reported",
			logs: []string{NoChecksPassedMsg},
			want: true,
		},
		{
			name: "still monitoring before checks pass",
			logs: []string{"monitoring CI for PR #42 (timeout: 4h)..."},
			want: false,
		},
		{
			name: "cleared when checks re-run",
			logs: []string{ChecksPassedMsg, ChecksRunningMsg},
			want: false,
		},
		{
			name: "cleared when a new failure appears",
			logs: []string{ChecksPassedMsg, "issues detected: test - auto-fixing (attempt 1/3)..."},
			want: false,
		},
		{
			name: "cleared when mergeability pending",
			logs: []string{ChecksPassedMsg, "mergeable state still pending: unknown"},
			want: false,
		},
		{
			name: "cleared when PR merged",
			logs: []string{ChecksPassedMsg, "PR has been merged!"},
			want: false,
		},
		{
			name: "ignores unrelated agent output",
			logs: []string{"CI failures detected: test failed", "agent says this is not ready to merge yet"},
			want: false,
		},
		{
			name: "empty",
			logs: nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ChecksPassed(tt.logs); got != tt.want {
				t.Errorf("ChecksPassed() = %v, want %v", got, tt.want)
			}
		})
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

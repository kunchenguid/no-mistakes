package types

import (
	"encoding/json"
	"testing"
)

func TestRunStatusTerminal(t *testing.T) {
	terminal := []RunStatus{RunCompleted, RunFailed, RunCancelled, RunChecksPassed, RunCIMonitorInterrupted, RunHandedOff}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("status %q: Terminal() = false, want true", s)
		}
	}
	nonTerminal := []RunStatus{RunPending, RunRunning, RunStatus("")}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Errorf("status %q: Terminal() = true, want false", s)
		}
	}
}

func TestAllStepsOrder(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 9 {
		t.Fatalf("expected 9 steps, got %d", len(steps))
	}

	expected := []StepName{StepIntent, StepRebase, StepReview, StepTest, StepDocument, StepLint, StepPush, StepPR, StepCI}
	for i, s := range steps {
		if s != expected[i] {
			t.Errorf("step[%d] = %q, want %q", i, s, expected[i])
		}
	}
}

func TestStepNameOrder(t *testing.T) {
	tests := []struct {
		step StepName
		want int
	}{
		{StepIntent, 1},
		{StepRebase, 2},
		{StepReview, 3},
		{StepTest, 4},
		{StepDocument, 5},
		{StepLint, 6},
		{StepPush, 7},
		{StepPR, 8},
		{StepCI, 9},
		{StepName("unknown"), 0},
	}

	for _, tt := range tests {
		if got := tt.step.Order(); got != tt.want {
			t.Errorf("%q.Order() = %d, want %d", tt.step, got, tt.want)
		}
	}
}

func TestStepNameUnmarshalJSON_LegacyBabysit(t *testing.T) {
	var step StepName
	if err := json.Unmarshal([]byte(`"babysit"`), &step); err != nil {
		t.Fatalf("unmarshal step name: %v", err)
	}
	if step != StepCI {
		t.Fatalf("step = %q, want %q", step, StepCI)
	}
}

func TestUserAbortStepError(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{"", "aborted by user"},
		{"   ", "aborted by user"},
		{"unsafe to land", "aborted by user: unsafe to land"},
		{"  padded  ", "aborted by user: padded"},
	} {
		got := UserAbortStepError(tc.reason)
		if got != tc.want {
			t.Errorf("UserAbortStepError(%q) = %q, want %q", tc.reason, got, tc.want)
		}
		if !IsUserAbortStepError(got) {
			t.Errorf("IsUserAbortStepError(%q) = false", got)
		}
	}
	for _, other := range []string{"", "aborted by users", "step review: aborted by user", "agent failed", "aborted by user:"} {
		if IsUserAbortStepError(other) {
			t.Errorf("IsUserAbortStepError(%q) = true", other)
		}
	}
}

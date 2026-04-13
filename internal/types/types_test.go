package types

import "testing"

func TestAllStepsOrder(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 7 {
		t.Fatalf("expected 7 steps, got %d", len(steps))
	}

	expected := []StepName{StepRebase, StepReview, StepTest, StepLint, StepPush, StepPR, StepCI}
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
		{StepRebase, 1},
		{StepReview, 2},
		{StepTest, 3},
		{StepLint, 4},
		{StepPush, 5},
		{StepPR, 6},
		{StepCI, 7},
		{StepName("unknown"), 0},
	}

	for _, tt := range tests {
		if got := tt.step.Order(); got != tt.want {
			t.Errorf("%q.Order() = %d, want %d", tt.step, got, tt.want)
		}
	}
}

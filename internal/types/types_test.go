package types

import "testing"

func TestAllStepsOrder(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 6 {
		t.Fatalf("expected 6 steps, got %d", len(steps))
	}

	expected := []StepName{StepReview, StepTest, StepLint, StepPush, StepPR, StepBabysit}
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
		{StepReview, 1},
		{StepTest, 2},
		{StepLint, 3},
		{StepPush, 4},
		{StepPR, 5},
		{StepBabysit, 6},
		{StepName("unknown"), 0},
	}

	for _, tt := range tests {
		if got := tt.step.Order(); got != tt.want {
			t.Errorf("%q.Order() = %d, want %d", tt.step, got, tt.want)
		}
	}
}

func TestStepOrdersAreConsecutive(t *testing.T) {
	steps := AllSteps()
	for i, s := range steps {
		if got := s.Order(); got != i+1 {
			t.Errorf("%q.Order() = %d, want %d", s, got, i+1)
		}
	}
}

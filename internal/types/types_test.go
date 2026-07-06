package types

import (
	"encoding/json"
	"testing"
)

func TestAllStepsOrder(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 10 {
		t.Fatalf("expected 10 steps, got %d", len(steps))
	}

	expected := []StepName{StepIntent, StepRebase, StepReview, StepImproveCodebase, StepTest, StepDocument, StepLint, StepPush, StepPR, StepCI}
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
		{StepImproveCodebase, 4},
		{StepTest, 5},
		{StepDocument, 6},
		{StepLint, 7},
		{StepPush, 8},
		{StepPR, 9},
		{StepCI, 10},
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

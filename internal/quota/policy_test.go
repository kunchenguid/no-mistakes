package quota

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStepClass_StatesTheRequirementForEveryStep pins the policy table: a new
// step must be classified here rather than inheriting silence, and the steps that
// judge or change code must ask for the strongest class.
func TestStepClass_StatesTheRequirementForEveryStep(t *testing.T) {
	want := map[types.StepName]Class{
		types.StepIntent:   ClassMedium,
		types.StepRebase:   ClassHigh,
		types.StepReview:   ClassHigh,
		types.StepTest:     ClassHigh,
		types.StepDocument: ClassMedium,
		types.StepLint:     ClassMedium,
		types.StepPush:     ClassMedium,
		types.StepPR:       ClassMedium,
		types.StepCI:       ClassHigh,
	}
	if len(stepNames) != len(want) {
		t.Fatalf("stepNames covers %d steps, the policy table %d", len(stepNames), len(want))
	}
	for _, step := range stepNames {
		class, ok := want[step]
		if !ok {
			t.Fatalf("step %s has no class in this test's table", step)
		}
		if got := StepClass(step); got != class {
			t.Errorf("StepClass(%s) = %s, want %s", step, got, class)
		}
	}
	// A custom gate shares its anchor's work under a name this package has never
	// seen, so it must fail closed rather than fall through to the cheapest class.
	if got := StepClass(types.StepName("dependency-audit")); got != ClassHigh {
		t.Errorf("an unknown step = %s, want the fail-closed class", got)
	}
}

func TestClassMeets(t *testing.T) {
	cases := []struct {
		have, want Class
		meets      bool
	}{
		{ClassHigh, ClassHigh, true},
		{ClassHigh, ClassMedium, true},
		{ClassHigh, ClassLow, true},
		{ClassMedium, ClassHigh, false},
		{ClassMedium, ClassMedium, true},
		{ClassLow, ClassMedium, false},
		{Class(""), ClassLow, false},
		{ClassHigh, Class(""), true},
	}
	for _, tc := range cases {
		if got := tc.have.Meets(tc.want); got != tc.meets {
			t.Errorf("%s.Meets(%s) = %v, want %v", tc.have, tc.want, got, tc.meets)
		}
	}
}

func TestBudgetsFor_UsesTheStepsOwnCeiling(t *testing.T) {
	budgets := Budgets{Default: 30 * time.Minute, Review: 45 * time.Minute, Test: 50 * time.Minute}
	cases := map[types.StepName]time.Duration{
		types.StepReview:                   45 * time.Minute,
		types.StepTest:                     50 * time.Minute,
		types.StepDocument:                 30 * time.Minute,
		types.StepName("dependency-audit"): 30 * time.Minute,
	}
	for step, want := range cases {
		if got := budgets.For(step); got != want {
			t.Errorf("For(%s) = %s, want %s", step, got, want)
		}
	}
	// A step-specific ceiling that is unset falls back to the default rather than
	// to a zero budget, which every runway would satisfy.
	unset := Budgets{Default: 20 * time.Minute}
	if got := unset.For(types.StepReview); got != 20*time.Minute {
		t.Errorf("For(review) = %s, want the default", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                    "0s",
		-1 * time.Second:     "0s",
		45 * time.Second:     "45s",
		29 * time.Minute:     "29m",
		time.Hour:            "1h0m",
		90 * time.Minute:     "1h30m",
		471945 * time.Second: "131h5m",
	}
	for input, want := range cases {
		if got := HumanDuration(input); got != want {
			t.Errorf("HumanDuration(%s) = %q, want %q", input, got, want)
		}
	}
}

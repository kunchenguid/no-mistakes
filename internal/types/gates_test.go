package types

import "testing"

func TestCustomGateStepName_RoundTrips(t *testing.T) {
	name := CustomGateStepName(StepTest, "mutation-budget")
	if name != StepName("gate:test:mutation-budget") {
		t.Fatalf("CustomGateStepName() = %q", name)
	}
	anchor, ok := name.CustomGateAnchor()
	if !ok || anchor != StepTest {
		t.Fatalf("CustomGateAnchor() = %q, %v; want test, true", anchor, ok)
	}
	if label := name.CustomGateLabel(); label != "mutation-budget" {
		t.Fatalf("CustomGateLabel() = %q", label)
	}
	if !name.IsCustomGate() {
		t.Fatal("IsCustomGate() = false, want true")
	}
}

// A gate runs immediately after its anchor, so it must share the anchor's
// order: a restart that resets from the anchor has to reset the gate with it.
func TestCustomGateStepName_OrdersWithItsAnchor(t *testing.T) {
	if got, want := CustomGateStepName(StepTest, "g").Order(), StepTest.Order(); got != want {
		t.Fatalf("gate order = %d, want the anchor's %d", got, want)
	}
	if got, want := CustomGateStepName(StepReview, "g").Order(), StepReview.Order(); got != want {
		t.Fatalf("gate order = %d, want the anchor's %d", got, want)
	}
}

// A malformed name must never be ordered as if it were a valid gate, or it
// would sort ahead of every real step at order 0 and reset the wrong rows.
func TestCustomGateAnchor_RejectsMalformedNames(t *testing.T) {
	for _, name := range []StepName{"gate:", "gate:test", "gate:test:", "gate:nope:g", "gate::g", StepTest} {
		if _, ok := name.CustomGateAnchor(); ok {
			t.Errorf("CustomGateAnchor(%q) reported a valid gate", name)
		}
	}
}

func TestIsCoreStepName(t *testing.T) {
	for _, step := range AllSteps() {
		if !IsCoreStepName(step) {
			t.Errorf("IsCoreStepName(%q) = false", step)
		}
	}
	if IsCoreStepName(CustomGateStepName(StepTest, "g")) {
		t.Error("a custom gate reported as a core step")
	}
}

package types

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCustomGateStepName_RoundTrips(t *testing.T) {
	name := CustomGateStepName(StepTest, "mutation-budget")
	if name != StepName("gate.test.mutation-budget") {
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

// A fabricated anchor must not be trusted to place a step, so a malformed name
// never resolves as a gate. That does send Order() to its default 0, which is
// why nothing may mint such a name in the first place: the encoding is only
// produced from a validated gate, and everything that reads one - ordering,
// the log path, the CLI - goes through this decoder.
func TestCustomGateAnchor_RejectsMalformedNames(t *testing.T) {
	for _, name := range []StepName{"gate.", "gate.test", "gate.test.", "gate.nope.g", "gate..g", StepTest} {
		if _, ok := name.CustomGateAnchor(); ok {
			t.Errorf("CustomGateAnchor(%q) reported a valid gate", name)
		}
	}
}

// The label is what an operator types at `axi logs --step`, and the step name
// is joined straight onto the run's log directory, so a label carrying a
// separator or a traversal segment must not decode as a gate at all.
func TestCustomGateAnchor_RejectsLabelsThatCouldEscapeALogPath(t *testing.T) {
	for _, name := range []StepName{
		"gate.test.../../../etc/passwd",
		`gate.test...\..\windows`,
		"gate.test.a/b",
		"gate.test.a.b",
		StepName("gate.test." + strings.Repeat("x", MaxCustomGateLabelLen+1)),
	} {
		if _, ok := name.CustomGateAnchor(); ok {
			t.Errorf("CustomGateAnchor(%q) reported a valid gate", name)
		}
		if label := name.CustomGateLabel(); label != "" {
			t.Errorf("CustomGateLabel(%q) = %q, want empty for a malformed name", name, label)
		}
	}
}

func TestValidCustomGateLabel(t *testing.T) {
	valid := []string{"a", "arch-fitness", "mutation-budget", "g1", strings.Repeat("x", MaxCustomGateLabelLen)}
	for _, label := range valid {
		if !ValidCustomGateLabel(label) {
			t.Errorf("ValidCustomGateLabel(%q) = false, want true", label)
		}
	}
	invalid := []string{"", "-lead", "trail-", "Upper", "has space", "has.dot", "has/slash", strings.Repeat("x", MaxCustomGateLabelLen+1)}
	for _, label := range invalid {
		if ValidCustomGateLabel(label) {
			t.Errorf("ValidCustomGateLabel(%q) = true, want false", label)
		}
	}
}

// The executor derives a step's log file directly from its step name, so a
// gate step name has to be a legal filename component on every supported
// platform. This reproduces the ':' regression: on Windows, Win32 parses
// '<name>:<stream>:<type>' as an NTFS alternate data stream, CreateFileW
// rejects "gate:test:mutation-budget.log" with ERROR_INVALID_NAME, and every
// run in a gates-configured repository fails at that step before the gate's
// own check ever runs. The reserved-character assertion runs on every platform
// so the defect cannot come back on a leg that would not fail natively.
func TestCustomGateStepName_IsUsableAsAStepLogFilename(t *testing.T) {
	filename := string(CustomGateStepName(StepTest, "mutation-budget")) + ".log"

	if strings.ContainsAny(filename, `<>:"/\|?*`) {
		t.Fatalf("step log filename %q contains a character Windows reserves in a path", filename)
	}
	for _, r := range filename {
		if r < 0x20 {
			t.Fatalf("step log filename %q contains a control character", filename)
		}
	}

	path := filepath.Join(t.TempDir(), filename)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create step log file %q: %v", filename, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close step log file: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat step log file: %v", err)
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

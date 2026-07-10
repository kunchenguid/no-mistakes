package steps

import (
	"os"
	"strings"
	"testing"
)

// TestRoutineCallersRouteThroughPurpose audits the gate-scoped routine callers:
// they must reach a native model only through the durable router (via a
// registered Purpose seam), never by constructing a native adapter themselves.
// Direct construction would bypass Profiles, Routes, provider circuits, and the
// invocation journal.
func TestRoutineCallersRouteThroughPurpose(t *testing.T) {
	// forbidden native-adapter constructors: a routine step must never build one.
	forbidden := []string{"agent.New(", "agent.NewWithOptions(", "agent.NewFallback(", "agent.NewLegacyInvoker("}
	cases := []struct {
		file string
		// seam is the routed entry each caller must use.
		seam string
	}{
		{"intent.go", "BindInvocation("},
		{"pr.go", "InvokeAgent("},
		{"test.go", "InvokeAgent("},
	}
	for _, tc := range cases {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		text := string(src)
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s constructs a native adapter directly (%q); routine work must route through a registered Purpose", tc.file, f)
			}
		}
		if !strings.Contains(text, tc.seam) {
			t.Errorf("%s does not reach the router through %q; it must not omit the Purpose seam", tc.file, tc.seam)
		}
	}
}

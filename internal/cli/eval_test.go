package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestEvalSetsIsLocalOnlyAndEmitsNoTelemetry(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	out, err := executeCmd("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v", err)
	}
	if !strings.Contains(out, "LOCAL-ONLY EVAL CASE SETS") {
		t.Fatalf("output = %q", out)
	}
	if recorder.count("command") != 0 || recorder.count("pageview") != 0 {
		t.Fatalf("eval emitted remote telemetry: %#v", recorder.events)
	}
}

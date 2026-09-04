package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestPonytailPushOptionRoundTrip(t *testing.T) {
	opt := formatPonytailPushOption(true)
	if opt != "no-mistakes.ponytail=full" {
		t.Fatalf("option = %q", opt)
	}
	required, err := parsePonytailPushOptions([]string{"other=value", opt})
	if err != nil || !required {
		t.Fatalf("round trip = %v, %v", required, err)
	}
	if formatPonytailPushOption(false) != "" {
		t.Fatal("disabled handoff emitted a push option")
	}
}

func TestPonytailPushOptionRejectsUnknownMode(t *testing.T) {
	_, err := parsePonytailPushOptions([]string{"no-mistakes.ponytail=lite"})
	if err == nil || !strings.Contains(err.Error(), `must be "full"`) {
		t.Fatalf("error = %v, want exact full-mode refusal", err)
	}
}

func TestConflictingActiveRunPonytail(t *testing.T) {
	legacy := &ipc.RunInfo{ID: "run-1"}
	if err := conflictingActiveRunPonytail(legacy, true); err == nil || !strings.Contains(err.Error(), "run-1") {
		t.Fatalf("required reattach error = %v", err)
	}
	if err := conflictingActiveRunPonytail(legacy, false); err != nil {
		t.Fatalf("ordinary reattach: %v", err)
	}
	required := &ipc.RunInfo{ID: "run-2", PonytailRequired: true}
	if err := conflictingActiveRunPonytail(required, true); err != nil {
		t.Fatalf("required reattach: %v", err)
	}
}

func TestRequiredPonytailIsMachineReadableInRunOutput(t *testing.T) {
	got := axiDoc(runObjectFieldWithKey("run", runView{
		ID: "run-1", Branch: "feature", Status: "running", HeadSHA: strings.Repeat("a", 40), PonytailRequired: true,
	}))
	if !strings.Contains(got, "ponytail: required") {
		t.Fatalf("run output omitted Ponytail requirement:\n%s", got)
	}
}

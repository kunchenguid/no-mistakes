package tui

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestHandoffRendering(t *testing.T) {
	run := testRun()
	run.Status = types.RunHandedOff
	m := NewModel("/tmp/sock", nil, run)
	if !m.done {
		t.Fatal("handed-off run is not done")
	}
	if !strings.Contains(m.terminalTitle(), "Handed off") {
		t.Fatalf("title = %s", m.terminalTitle())
	}
	banner := renderOutcomeBanner(run, nil)
	if !strings.Contains(banner, "handed off for human review") || strings.Contains(banner, "cancelled") {
		t.Fatalf("banner = %s", banner)
	}
	run.TestOverrideReason = "operator accepted inconclusive validation"
	banner = renderOutcomeBanner(run, nil)
	if !strings.Contains(banner, "handed off with override") || !strings.Contains(banner, run.TestOverrideReason) {
		t.Fatalf("override lost: %s", banner)
	}
}

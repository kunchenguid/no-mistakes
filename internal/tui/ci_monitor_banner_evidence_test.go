package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestEvidence_CIMonitorInterruptedBanner renders the TUI outcome banner and
// status line for the RunCIMonitorInterrupted terminal status (issue #361) and
// prints them with raw ANSI so the yellow styling is reviewer-visible when the
// captured bytes are written to a terminal.
func TestEvidence_CIMonitorInterruptedBanner(t *testing.T) {
	run := testRun()
	run.Status = types.RunCIMonitorInterrupted
	steps := []ipc.StepResultInfo{
		{StepName: types.StepPR, Status: types.StepStatusCompleted},
		{StepName: types.StepCI, Status: types.StepStatusSkipped},
	}

	banner := renderOutcomeBanner(run, steps)
	statusLine := runStatusStyled(run.Status)

	if plain := stripANSI(banner); !strings.Contains(plain, "CI monitor interrupted") {
		t.Fatalf("banner missing 'CI monitor interrupted', got: %q", plain)
	}
	if plain := stripANSI(statusLine); !strings.Contains(plain, "ci_monitor_interrupted") {
		t.Fatalf("status line missing 'ci_monitor_interrupted', got: %q", plain)
	}

	var b strings.Builder
	fmt.Fprintln(&b, "TUI status line: "+statusLine)
	fmt.Fprintln(&b, "TUI outcome banner: "+banner)
	t.Logf("\n%s", b.String())
}

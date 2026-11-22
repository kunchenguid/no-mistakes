package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
)

func TestAvailableHeadRecoveryGuidanceFitsTerminal(t *testing.T) {
	state := branchsync.State{
		State:      branchsync.StatePipelineOwned,
		Safety:     "blocked_pipeline_owned_recoverable",
		NextAction: &branchsync.NextAction{Code: "recover_custody"},
		Recovery:   &branchsync.RecoveryEvidence{Source: "available_head", KeepLocal: true, Proof: "verified"},
	}
	for _, width := range []int{40, 80, 140} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			view := stripANSI(renderLocalBranchStatus(&state, false, width))
			var content []string
			for _, line := range strings.Split(view, "\n") {
				// A terminal clips content outside its cell grid. The safety
				// explanation must fit, not merely exist in the output bytes.
				if got := ansi.StringWidth(line); got > width {
					t.Errorf("rendered line is %d cells wide in a %d-cell terminal: %q", got, width, line)
				}
				if strings.HasPrefix(line, "│") {
					content = append(content, strings.Trim(line, "│ "))
				}
			}
			text := strings.Join(strings.Fields(strings.Join(content, " ")), " ")
			for _, want := range []string{"preserve the pipeline commits at the recovery ref", "keep the current local head", "unpublished commits remain off the branch"} {
				if !strings.Contains(text, want) {
					t.Errorf("visible guidance missing %q: %s", want, text)
				}
			}
			if !strings.Contains(view, "u recover custody") {
				t.Errorf("recovery action missing: %s", view)
			}
		})
	}
}

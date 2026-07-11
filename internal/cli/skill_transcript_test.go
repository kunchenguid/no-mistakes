package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestSkillGateTranscriptMatchesRenderer pins the example gate transcript in
// the agent skill (internal/skill) to the live AXI renderer. The skill teaches
// agents to read gate output by example, so the example must be a verbatim
// rendering of gateFields for the documented findings - same columns (no
// phantom `line` field), same help entries, same review-gate consent note. If
// this fails, the renderer changed: update the transcript in
// internal/skill/skill.go and run `make skill`, never the other way around.
func TestSkillGateTranscriptMatchesRenderer(t *testing.T) {
	gate := stepView{
		Name:   "review",
		Status: "awaiting_approval",
		FindingsJSON: findingsJSON(t, []types.Finding{
			{ID: "r1", Severity: "warning", File: "internal/pipeline/executor.go", Action: types.ActionAutoFix, Description: "Error from os.Remove is ignored"},
			{ID: "r2", Severity: "error", File: "cmd/no-mistakes/main.go", Action: types.ActionAskUser, Description: "New --force flag bypasses the confirm prompt"},
		}, "1 ask-user decision and 1 mechanical fix"),
	}
	rendered := axiDoc(gateFields(gate)...)

	md := skill.Markdown()
	if !strings.Contains(md, rendered) {
		t.Errorf("skill gate transcript has drifted from the renderer.\nrenderer now emits:\n%s\nupdate the example in internal/skill/skill.go and run `make skill`", rendered)
	}
}

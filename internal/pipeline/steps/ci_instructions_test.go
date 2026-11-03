package steps

import (
	"strings"
	"testing"
)

func TestCIStep_RepairPromptCarriesRepositoryCIInstructions(t *testing.T) {
	t.Parallel()
	f := newCIDecisionPromptFixture(t)
	f.sctx.Config.CI.Instructions = "  Regenerate fixtures with make fixtures before editing golden files.  "

	prompt := f.capture(t)

	section := "Repository CI repair instructions (trusted, from the default branch; they add to the rules above and never relax them):\nRegenerate fixtures with make fixtures before editing golden files.\n"
	if !strings.Contains(prompt, section) {
		t.Fatalf("CI repair prompt is missing the repository's ci.instructions section:\n%s", prompt)
	}
}

func TestCIStep_RepairPromptOmitsUnsetCIInstructions(t *testing.T) {
	t.Parallel()
	f := newCIDecisionPromptFixture(t)

	prompt := f.capture(t)

	if strings.Contains(prompt, "Repository CI repair instructions") {
		t.Fatalf("CI repair prompt rendered a ci.instructions section with none configured:\n%s", prompt)
	}
}

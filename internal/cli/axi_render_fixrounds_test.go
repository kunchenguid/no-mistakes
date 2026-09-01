package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A review gate under review.max_fix_rounds shows the budget so the driving
// agent knows before answering whether --action fix is still available.
func TestWriteGateShape_FixRoundsBudget(t *testing.T) {
	findings := findingsJSON(t, []types.Finding{
		{ID: "review-1", Severity: "error", File: "main.go", Line: 4, Action: types.ActionAutoFix, Description: "bug"},
	}, "1 blocking issue")

	remaining := stepView{Name: "review", Status: "fix_review", FindingsJSON: findings, FixRoundCount: 1, MaxFixRounds: 2}
	out := axiDoc(gateFields(remaining)...)
	if !strings.Contains(out, "fix_rounds: 1/2 used\n") {
		t.Fatalf("gate with budget remaining:\n%s", out)
	}

	spent := stepView{Name: "review", Status: "fix_review", FindingsJSON: findings, FixRoundCount: 2, MaxFixRounds: 2}
	out = axiDoc(gateFields(spent)...)
	// The long value is quoted by the encoder; assert on the content.
	for _, want := range []string{`fix_rounds: "2/2 used`, "further fix refused", "--action approve, skip, or abort"} {
		if !strings.Contains(out, want) {
			t.Fatalf("exhausted gate missing %q:\n%s", want, out)
		}
	}

	unbounded := stepView{Name: "review", Status: "awaiting_approval", FindingsJSON: findings, FixRoundCount: 3}
	if out := axiDoc(gateFields(unbounded)...); strings.Contains(out, "fix_rounds") {
		t.Fatalf("no cap configured: fix_rounds must not be rendered:\n%s", out)
	}
}

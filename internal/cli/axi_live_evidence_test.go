package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiRunObjectNamesTheLiveEvidencePath: an agent reading a green Test step
// cannot otherwise tell a freshly driven live-evidence turn from a reused
// verdict or a diff with no product file in it, and that difference is exactly
// what it needs to decide whether to trust the step.
func TestAxiRunObjectNamesTheLiveEvidencePath(t *testing.T) {
	testStep := func(findings types.Findings) stepView {
		raw, err := json.Marshal(findings)
		if err != nil {
			t.Fatal(err)
		}
		return stepView{Name: string(types.StepTest), Status: "completed", FindingsJSON: string(raw)}
	}

	for _, tc := range []struct {
		name string
		rv   runView
		want string
	}{
		{
			name: "reused verdict",
			rv: runView{Steps: []stepView{testStep(types.Findings{
				Verdict:        types.TestVerdictGo,
				EvidenceSource: types.TestEvidenceSourceReused,
				EvidenceReason: "product files unchanged since abc123; reused from run run-9",
			})}},
			want: "reused: product files unchanged since abc123; reused from run run-9",
		},
		{
			name: "no product file in diff",
			rv: runView{Steps: []stepView{testStep(types.Findings{
				Verdict:        types.TestVerdictNoSurface,
				EvidenceSource: types.TestEvidenceSourceNoProductChange,
				EvidenceReason: "no product file in diff",
			})}},
			want: "no-product-change: no product file in diff",
		},
		{
			name: "agent drove the scenarios",
			rv: runView{Steps: []stepView{testStep(types.Findings{
				Verdict:        types.TestVerdictGo,
				EvidenceSource: types.TestEvidenceSourceAgent,
				EvidenceReason: "live-evidence agent drove scenarios in this run",
			})}},
			want: "agent: live-evidence agent drove scenarios in this run",
		},
		{
			name: "recorded before the gate existed",
			rv:   runView{Steps: []stepView{testStep(types.Findings{Verdict: types.TestVerdictGo})}},
			want: "",
		},
		{
			name: "no test step",
			rv:   runView{Steps: []stepView{{Name: string(types.StepReview), Status: "completed"}}},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rv.liveEvidenceSource(); got != tc.want {
				t.Fatalf("liveEvidenceSource() = %q, want %q", got, tc.want)
			}
			rendered := axiDoc(runObjectField(tc.rv))
			if tc.want == "" {
				if strings.Contains(rendered, "live_evidence") {
					t.Fatalf("run object should carry no live_evidence field:\n%s", rendered)
				}
				return
			}
			if !strings.Contains(rendered, "live_evidence") || !strings.Contains(rendered, tc.want) {
				t.Fatalf("run object omits the live-evidence path:\n%s", rendered)
			}
		})
	}
}

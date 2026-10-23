package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/quotarouting"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// quotaRoutingRenderRecord builds the record a run setup persists for a run
// that ranked codex above claude, then switched once mid-run.
func quotaRoutingRenderRecord(t *testing.T, withSwitch bool) *quotarouting.Record {
	t.Helper()
	remaining := 80.0
	priority := 80.0
	record := quotarouting.NewRecord(true,
		[]types.AgentName{types.AgentClaude, types.AgentCodex},
		[]types.AgentName{types.AgentCodex, types.AgentClaude},
		quotarouting.Selection{
			Choice: types.AgentCodex,
			Order:  []types.AgentName{types.AgentCodex, types.AgentClaude},
			Reason: "ranked by spendPriority",
			Evidence: []quotarouting.Evidence{
				{Agent: types.AgentCodex, Provider: "codex", Account: "codex-home", Scope: "all_models",
					Remaining: &remaining, SpendPriority: &priority, Runway: "through_reset",
					Eligible: true, Rankable: true, Reason: "80% remaining"},
				{Agent: types.AgentClaude, Provider: "claude", Scope: "all_models",
					Runway: "unknown", Eligible: true, Rankable: false,
					Reason: "runway unknown; kept after ranked candidates"},
			},
		})
	if withSwitch {
		record = record.AppendSwitch(types.AgentCodex, types.AgentClaude, "codex exhausted its quota", time.Unix(1792270231, 0))
	}
	return record
}

// TestAxiStatusRendersQuotaRouting pins the local-only run-stats surface: the
// choice, the order, the per-candidate evidence, and any mid-run switches all
// render under one quota_routing object, and a run without a record renders
// none.
func TestAxiStatusRendersQuotaRouting(t *testing.T) {
	payload, err := json.Marshal(quotaRoutingRenderRecord(t, true))
	if err != nil {
		t.Fatal(err)
	}
	view := runViewFromIPC(&ipc.RunInfo{ID: "run-1", QuotaRouting: string(payload)})
	var out bytes.Buffer
	render := &cobra.Command{}
	render.SetOut(&out)
	emitDoc(render, runObjectField(view))
	rendered := out.String()
	for _, want := range []string{
		"quota_routing:",
		"choice: codex",
		`order: "codex, claude"`,
		"reason: ranked by spendPriority",
		"candidates[2]{agent,provider,account,scope,remaining,spend_priority,runway,status,reason}:",
		"codex,codex,codex-home,all_models,80%,",
		"through_reset,ranked,80% remaining",
		`claude,claude,"",all_models,`,
		"unknown,unranked,runway unknown; kept after ranked candidates",
		"switches[1]{from,to,reason}:",
		"codex,claude,codex exhausted its quota",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("status missing %q:\n%s", want, rendered)
		}
	}

	// A run without a record (feature off or pre-feature) renders no
	// quota_routing object at all.
	var plain bytes.Buffer
	plainRender := &cobra.Command{}
	plainRender.SetOut(&plain)
	emitDoc(plainRender, runObjectField(runViewFromIPC(&ipc.RunInfo{ID: "run-2"})))
	if strings.Contains(plain.String(), "quota_routing") {
		t.Errorf("a run without a record must not render one:\n%s", plain.String())
	}
}

// TestAxiStatusUnreadableQuotaRoutingRecordIsWarnedNotFatal pins the recovery
// posture for a corrupt stored payload: the render drops the field and the
// rest of the status still renders.
func TestAxiStatusUnreadableQuotaRoutingRecordIsWarnedNotFatal(t *testing.T) {
	view := runViewFromIPC(&ipc.RunInfo{ID: "run-3", QuotaRouting: "{corrupt"})
	var out bytes.Buffer
	render := &cobra.Command{}
	render.SetOut(&out)
	emitDoc(render, runObjectField(view))
	if strings.Contains(out.String(), "quota_routing") {
		t.Errorf("a corrupt record must not render:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "id: run-3") {
		t.Errorf("the rest of the status must still render:\n%s", out.String())
	}
}

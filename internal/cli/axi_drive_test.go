package cli

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGateResolution(t *testing.T) {
	tests := []struct {
		name         string
		gate         stepView
		alreadyFixed bool
		wantAction   types.ApprovalAction
		wantIDs      []string
	}{
		{
			name: "actionable findings are fixed with every finding selected",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"warning","description":"design choice","action":"ask-user"},{"id":"review-2","severity":"info","description":"fyi","action":"no-op"}],"summary":"2"}`,
			},
			wantAction: types.ActionFix,
			wantIDs:    []string{"review-1", "review-2"},
		},
		{
			name: "only non-actionable findings are approved",
			gate: stepView{
				Name:         "test",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"id":"test-1","severity":"info","description":"fyi","action":"no-op"}],"summary":"1"}`,
			},
			wantAction: types.ActionApprove,
		},
		{
			name: "no findings are approved",
			gate: stepView{
				Name:         "push",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: ``,
			},
			wantAction: types.ActionApprove,
		},
		{
			name: "already fixed step is approved (no fix loop)",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusFixReview),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"warning","description":"still here","action":"ask-user"}],"summary":"1"}`,
			},
			alreadyFixed: true,
			wantAction:   types.ActionApprove,
		},
		{
			name: "reattached fix review is approved without in-memory fix state",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusFixReview),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"warning","description":"still here","action":"ask-user"}],"summary":"1"}`,
			},
			wantAction: types.ActionApprove,
		},
		{
			name: "actionable findings without ids are approved rather than fixing nothing",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"severity":"warning","description":"no id","action":"ask-user"}],"summary":"1"}`,
			},
			wantAction: types.ActionApprove,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, ids := gateResolution(tt.gate, tt.alreadyFixed)
			t.Logf("auto-resolution action=%s finding_ids=%v", action, ids)
			if action != tt.wantAction {
				t.Fatalf("action = %s, want %s", action, tt.wantAction)
			}
			if len(ids) != len(tt.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tt.wantIDs)
			}
			for i := range ids {
				if ids[i] != tt.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", ids, tt.wantIDs)
				}
			}
		})
	}
}

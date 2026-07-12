package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestApprovalActionForGateRejectsMalformedAddedFindingBeforeJournalInput(t *testing.T) {
	gate := &db.ApprovalGate{
		ID:            "gate-1",
		RunID:         "run-1",
		StepResultID:  "step-1",
		SourceRoundID: "round-1",
		FindingsJSON:  `{"findings":[],"summary":"none"}`,
	}
	tests := []struct {
		name    string
		finding types.Finding
		want    string
	}{
		{name: "missing severity", finding: types.Finding{Description: "fix it", Action: types.ActionAutoFix}, want: "requires severity and description"},
		{name: "missing description", finding: types.Finding{Severity: "warning", Action: types.ActionAutoFix}, want: "requires severity and description"},
		{name: "non-fix action", finding: types.Finding{Severity: "warning", Description: "fix it", Action: types.ActionNoOp}, want: "invalid action"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, input, err := approvalActionForGate(gate, types.ActionFix, nil, nil, []types.Finding{tt.finding})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("approvalActionForGate error = %v, want %q", err, tt.want)
			}
			if input.GateID != "" {
				t.Fatalf("malformed finding produced journal input: %+v", input)
			}
		})
	}
}

func TestApprovalActionForGateAcceptsCLIAddedFindingDefault(t *testing.T) {
	gate := &db.ApprovalGate{
		ID:            "gate-1",
		RunID:         "run-1",
		StepResultID:  "step-1",
		SourceRoundID: "round-1",
		FindingsJSON:  `{"findings":[],"summary":"none"}`,
	}
	finding := types.Finding{Severity: "warning", Description: "...", Action: types.ActionAutoFix}

	response, input, err := approvalActionForGate(gate, types.ActionFix, nil, nil, []types.Finding{finding})
	if err != nil {
		t.Fatalf("approvalActionForGate rejected CLI-defaulted finding: %v", err)
	}
	if len(response.addedFindings) != 1 || response.addedFindings[0] != finding {
		t.Fatalf("approval response added findings = %+v, want %+v", response.addedFindings, finding)
	}
	if !strings.Contains(input.AddedFindingsJSON, `"severity":"warning"`) {
		t.Fatalf("journal input did not preserve CLI severity default: %s", input.AddedFindingsJSON)
	}
}

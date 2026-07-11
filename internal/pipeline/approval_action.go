package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func approvalActionForGate(gate *db.ApprovalGate, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) (approvalResponse, db.ApprovalActionInput, error) {
	if gate == nil {
		return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("approval response has no durable gate")
	}
	ids := append([]string(nil), findingIDs...)
	instructionCopy := make(map[string]string, len(instructions))
	for id, instruction := range instructions {
		instructionCopy[id] = instruction
	}
	addedCopy := append([]types.Finding(nil), addedFindings...)
	if action == types.ActionFix {
		findings, err := types.ParseFindingsJSON(gate.FindingsJSON)
		if err != nil {
			return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("parse approval gate findings: %w", err)
		}
		byID := make(map[string]types.Finding, len(findings.Items))
		for _, finding := range findings.Items {
			if finding.ID == "" {
				return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("approval gate contains a finding without an id")
			}
			if _, duplicate := byID[finding.ID]; duplicate {
				return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("approval gate contains duplicate finding id %q", finding.ID)
			}
			byID[finding.ID] = finding
		}
		if len(ids) == 0 && len(addedCopy) == 0 {
			for _, finding := range findings.Items {
				if finding.Action != types.ActionNoOp {
					ids = append(ids, finding.ID)
				}
			}
		}
		seen := make(map[string]struct{}, len(ids))
		unique := ids[:0]
		for _, id := range ids {
			if _, ok := byID[id]; !ok {
				return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("approval response selected unknown finding %q", id)
			}
			if byID[id].Action == types.ActionNoOp {
				return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("approval response selected non-actionable finding %q", id)
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
		ids = unique
		for id := range instructionCopy {
			if _, selected := seen[id]; !selected {
				return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("approval instructions reference unselected finding %q", id)
			}
		}
		for index, finding := range addedCopy {
			if strings.TrimSpace(finding.Description) == "" || strings.TrimSpace(finding.Severity) == "" {
				return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("user-added finding %d requires severity and description", index+1)
			}
			if finding.Action != types.ActionAutoFix && finding.Action != types.ActionAskUser {
				return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("user-added finding %d has invalid action %q", index+1, finding.Action)
			}
		}
	} else if len(ids) != 0 || len(instructionCopy) != 0 || len(addedCopy) != 0 {
		return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("approval action %q does not accept a fix payload", action)
	}

	selectedJSON, err := json.Marshal(ids)
	if err != nil {
		return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("marshal selected finding ids: %w", err)
	}
	instructionsJSON, err := json.Marshal(instructionCopy)
	if err != nil {
		return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("marshal approval instructions: %w", err)
	}
	addedJSON, err := json.Marshal(addedCopy)
	if err != nil {
		return approvalResponse{}, db.ApprovalActionInput{}, fmt.Errorf("marshal user-added findings: %w", err)
	}
	response := approvalResponse{action: action, findingIDs: ids, instructions: instructionCopy, addedFindings: addedCopy}
	input := db.ApprovalActionInput{
		GateID: gate.ID, RunID: gate.RunID, StepResultID: gate.StepResultID, StepRoundID: gate.SourceRoundID,
		Action: action, SelectedFindingIDsJSON: string(selectedJSON), InstructionsJSON: string(instructionsJSON), AddedFindingsJSON: string(addedJSON),
	}
	return response, input, nil
}

func approvalResponseFromRecord(action *db.ApprovalAction) (approvalResponse, error) {
	if action == nil {
		return approvalResponse{}, fmt.Errorf("approval action is nil")
	}
	response := approvalResponse{actionID: action.ID, action: action.Action}
	if err := json.Unmarshal([]byte(action.SelectedFindingIDsJSON), &response.findingIDs); err != nil {
		return approvalResponse{}, fmt.Errorf("decode approval selected finding ids: %w", err)
	}
	if err := json.Unmarshal([]byte(action.InstructionsJSON), &response.instructions); err != nil {
		return approvalResponse{}, fmt.Errorf("decode approval instructions: %w", err)
	}
	if err := json.Unmarshal([]byte(action.AddedFindingsJSON), &response.addedFindings); err != nil {
		return approvalResponse{}, fmt.Errorf("decode user-added findings: %w", err)
	}
	return response, nil
}

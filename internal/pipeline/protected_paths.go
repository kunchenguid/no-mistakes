package pipeline

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const protectedPathFindingID = "protected-path-refusal"

// HasProtectedPathRefusal identifies gates that require an explicit response.
func HasProtectedPathRefusal(findingsJSON string) bool {
	return hasFindingID(findingsJSON, protectedPathFindingID)
}

// HasUnvalidatedWorkRefusal identifies a Test budget-cut gate whose worktree
// holds work no Test turn validated. Approve is refused there because the
// steps after Test would commit and publish that work; fix validates it.
func HasUnvalidatedWorkRefusal(findingsJSON string) bool {
	return hasFindingID(findingsJSON, types.FindingIDTestAgentUnvalidatedWork)
}

func hasFindingID(findingsJSON, id string) bool {
	findings, _ := types.ParseFindingsJSON(findingsJSON)
	for _, finding := range findings.Items {
		if finding.ID == id {
			return true
		}
	}
	return false
}

// approvalRefusal reports why Approve is rejected at a gate, or "" when it is
// accepted.
func approvalRefusal(step types.StepName, findingsJSON string) string {
	switch {
	case HasProtectedPathRefusal(findingsJSON):
		return fmt.Sprintf("cannot approve a protected-path refusal: resolve the reported edit, then use fix to retry %s; approval would skip unfinished work", step)
	case HasUnvalidatedWorkRefusal(findingsJSON):
		return fmt.Sprintf("cannot approve %s: the run worktree holds work no Test turn validated and approval would publish it; inspect it as the findings describe, then use fix to validate it, or abort", step)
	}
	return ""
}

type ProtectedPathError struct {
	Path string
	Rule string
}

func (e *ProtectedPathError) Error() string {
	return fmt.Sprintf("refusing automatic commit: dirty protected path %q matches protected_paths rule %q; index and worktree preserved, inspect and resolve the edit before retrying", e.Path, e.Rule)
}

func ProtectedPathOutcome(err error) *StepOutcome {
	var refusal *ProtectedPathError
	if !errors.As(err, &refusal) {
		return nil
	}
	findings, _ := types.MarshalFindingsJSON(types.Findings{
		Summary: "Automatic commit refused for a protected path",
		Items: []types.Finding{{
			ID:          protectedPathFindingID,
			Severity:    "error",
			File:        refusal.Path,
			Description: err.Error(),
			Action:      types.ActionAskUser,
		}},
	})
	return &StepOutcome{NeedsApproval: true, Findings: findings}
}

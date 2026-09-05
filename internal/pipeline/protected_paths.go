package pipeline

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const protectedPathFindingID = "protected-path-refusal"

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

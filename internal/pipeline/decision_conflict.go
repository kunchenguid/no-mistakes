package pipeline

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DecisionConflictError refuses a repair and parks its named findings at the
// existing approval gate. Local edits remain available for an explicit repair.
type DecisionConflictError struct {
	Findings types.Findings
	// CheckedTreeSHA is the worktree tree the check inspected.
	CheckedTreeSHA string
}

func (e *DecisionConflictError) Error() string {
	var descriptions []string
	for _, f := range e.Findings.Items {
		descriptions = append(descriptions, f.Description)
	}
	return "repair contradicts recorded fix decision: " + strings.Join(descriptions, "; ")
}

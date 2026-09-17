package pipeline

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const protectedPathFindingID = "protected-path-refusal"

// HasProtectedPathRefusal identifies gates that require an explicit response.
func HasProtectedPathRefusal(findingsJSON string) bool {
	findings, _ := types.ParseFindingsJSON(findingsJSON)
	for _, finding := range findings.Items {
		if finding.ID == protectedPathFindingID {
			return true
		}
	}
	return false
}

// HasUnansweredReviewQuestion identifies gates an automatic resolver must leave
// alone because only an answer can settle them.
//
// It is the JSON-string sibling of types.HasReviewQuestion, and it exists so
// that EVERY auto-resolve path reads one predicate instead of repeating the
// parse-then-check pair. There are two such paths - `axi`'s --yes and the TUI's
// yolo mode - and the carve-out first landed on only one of them, which left the
// stated property ("--yes leaves an open question awaiting an explicit answer")
// true of `axi` and false of the TUI. A shared predicate is what makes a third
// path inherit the rule rather than reintroduce the bug.
//
// It deliberately does NOT constrain a human's explicit approve or fix: the
// design permits resolving a gate over an open question knowingly. Only the
// automatic paths stand aside.
func HasUnansweredReviewQuestion(findingsJSON string) bool {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return false
	}
	return types.HasReviewQuestion(findings)
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

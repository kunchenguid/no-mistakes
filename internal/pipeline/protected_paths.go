package pipeline

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const protectedPathFindingID = "protected-path-refusal"

// ReviewQuestionsUnreadableFindingID is the single synthetic finding the review
// step emits when the reviewer's question history could not be read in full.
// It is one fixed finding rather than a class of them, so it is keyed by ID
// exactly as the protected-path refusal above is, and deliberately carries no
// review-question category: that category summons the answer-first gate help,
// and an answer is precisely what the daemon refuses while the history is
// unreadable.
const ReviewQuestionsUnreadableFindingID = "review-questions-unreadable"

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

// HasUnreadableReviewQuestionHistory identifies gates an automatic resolver
// must leave alone because the reviewer's question history could not be read in
// full, so the decision this gate asks for cannot be made from the findings.
//
// Without it the marker is an ordinary actionable ask-user finding: gateResolution
// selects its id and returns ActionFix, the fixer is handed "decide this gate
// yourself" as work it cannot do, the rereview re-emits the identical marker,
// and the resulting fix_review gate is approved as already-fixed - so a
// possibly-dropped major question reaches nobody and the park costs a fix round
// instead of buying a human decision. Same carve-out shape, and the same one
// predicate for every automatic path, as HasUnansweredReviewQuestion above; a
// human's own approve, fix or skip stays allowed.
func HasUnreadableReviewQuestionHistory(findingsJSON string) bool {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return false
	}
	for _, finding := range findings.Items {
		if finding.ID == ReviewQuestionsUnreadableFindingID {
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

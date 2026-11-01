package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// HasReviewFixRoundLimit identifies an exhausted Review budget. Automatic
// resolvers must leave this gate for an explicit decision, never approve it.
func HasReviewFixRoundLimit(raw string) bool {
	return hasFindingID(raw, types.FindingIDReviewFixRoundsExhausted)
}

func reviewFixRoundCount(rounds []*db.StepRound) int {
	count := 0
	for _, round := range rounds {
		if round.IsFixRound() {
			count++
		}
	}
	return count
}

func (e *Executor) reviewFixRoundLimit(stepID string) (int, error) {
	step, err := e.db.GetStepResult(stepID)
	if err != nil {
		return 0, err
	}
	if step == nil {
		return 0, fmt.Errorf("Review step not found")
	}
	// An active step from before the feature was unbounded. A config edit
	// must not retroactively impose a limit on it during recovery.
	if step.ReviewMaxFixRounds == nil {
		return 0, nil
	}
	if *step.ReviewMaxFixRounds < 0 {
		return 0, fmt.Errorf("invalid pinned Review fix-round limit")
	}
	return *step.ReviewMaxFixRounds, nil
}

func reviewFixRoundLimitMessage(used, limit int) string {
	return fmt.Sprintf("fix_rounds_exhausted: Review used %d/%d fix rounds; the gate remains parked with its remaining findings", used, limit)
}

func reviewFixRoundLimitRefusal(used, limit int, gateFindings string) *RespondRefusal {
	help := "Inspect the remaining findings and explicitly approve, skip, or abort. A new run gets a new budget; changing config does not extend this run."
	if HasProtectedPathRefusal(gateFindings) {
		help = "Approve is also rejected because this gate holds a protected-path refusal: skip this step or abort the run. A new run gets a new budget; changing config does not extend this run."
	}
	return &RespondRefusal{Message: reviewFixRoundLimitMessage(used, limit), Help: help}
}

func appendReviewFixRoundLimit(raw string, used, limit int) (string, error) {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return "", err
	}
	findings.Items = append(findings.Items, types.Finding{
		ID:          types.FindingIDReviewFixRoundsExhausted,
		Severity:    "warning",
		Action:      types.ActionAskUser,
		Description: reviewFixRoundLimitMessage(used, limit),
	})
	return types.MarshalFindingsJSON(findings)
}

package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// gatePolicy is the review-loop policy the executor applies to one step
// (config.Review). Every other step, and a review step with no policy
// configured, gets the zero policy, which reproduces the pre-policy behavior
// exactly: an unbounded fix loop, only "auto-fix" findings fixed
// automatically, and a park on any ask-user finding or NeedsApproval outcome.
type gatePolicy struct {
	review       bool
	maxFixRounds int
	fixAskUser   bool
	gateSeverity string
}

func gatePolicyFor(step types.StepName, cfg *config.Config) gatePolicy {
	if step != types.StepReview || cfg == nil {
		return gatePolicy{}
	}
	return gatePolicy{
		review:       true,
		maxFixRounds: cfg.Review.MaxFixRounds,
		fixAskUser:   cfg.Review.AutoFixAskUser,
		gateSeverity: cfg.Review.GateSeverity,
	}
}

// fixRoundsRemain reports whether another fix round - automatic or
// gate-driven - is allowed after `used` fix rounds. Unbounded at 0.
func (p gatePolicy) fixRoundsRemain(used int) bool {
	return p.maxFixRounds <= 0 || used < p.maxFixRounds
}

// errorOnly reports whether only error-severity findings park this step
// (review.gate_severity: error).
func (p gatePolicy) errorOnly() bool {
	return p.review && p.gateSeverity == config.ReviewGateSeverityError
}

// fixableFindingsJSON selects the findings an automatic fix round works on.
// Default: "auto-fix" findings at every severity (the pre-policy rule). With
// auto_fix_ask_user, "ask-user" findings join them; "no-op" never does. Under
// gate_severity: error the budget is spent only on findings that would
// otherwise park, so warnings and info are left as report-only.
func (p gatePolicy) fixableFindingsJSON(raw string) string {
	if !p.fixAskUser && !p.errorOnly() {
		return autoFixableFindingsJSON(raw)
	}
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	fixable := types.FilterFindingsBy(findings, func(item types.Finding) bool {
		switch item.ActionOrDefault() {
		case types.ActionAutoFix:
		case types.ActionAskUser:
			if !p.fixAskUser {
				return false
			}
		default:
			return false
		}
		return !p.errorOnly() || types.NormalizeFindingSeverity(item.Severity) == types.FindingSeverityError
	})
	if len(fixable.Items) == 0 {
		return ""
	}
	fixableRaw, err := types.MarshalFindingsJSON(fixable)
	if err != nil {
		return raw
	}
	return fixableRaw
}

// parksOnFindingsJSON reports whether the findings themselves force a park
// once no automatic fix round will run. Default: any ask-user finding (an
// unclassified finding defaults to ask-user and so parks, fail-closed). Under
// gate_severity: error, any error finding parks regardless of action, and
// nothing below error does.
func (p gatePolicy) parksOnFindingsJSON(raw string) bool {
	if !p.errorOnly() {
		return hasAskUserFindingsJSON(raw)
	}
	if raw == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if types.NormalizeFindingSeverity(item.Severity) == types.FindingSeverityError {
			return true
		}
	}
	return false
}

// fixBudget is the fix-round budget recorded when a step parks, so a gate
// response can be refused without re-deriving it from the database.
type fixBudget struct {
	used int
	max  int
}

func (b fixBudget) exhausted() bool { return b.max > 0 && b.used >= b.max }

// FixRoundsExhaustedError is returned by Respond when the parked step has
// used its whole review.max_fix_rounds budget and the response asked for
// another fix. The step stays parked; approve, skip, and abort still work.
type FixRoundsExhaustedError struct {
	Step types.StepName
	Used int
	Max  int
}

// Error starts with types.FixRoundsExhaustedCode so the code survives the
// IPC error's plain message and a driver can match on it.
func (e *FixRoundsExhaustedError) Error() string {
	return fmt.Sprintf("%s: %s fix rounds exhausted (%d/%d, review.max_fix_rounds); a further fix is refused at this gate - respond with --action approve, skip, or abort, or raise review.max_fix_rounds and rerun", types.FixRoundsExhaustedCode, e.Step, e.Used, e.Max)
}

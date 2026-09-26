package pipeline

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RepeatFindingFindingID is the single synthetic ask-user finding a step's gate
// carries when the fixer stopped because a finding a fix round was already
// dispatched for (by auto-fix or by an operator's fix response) was reported
// again. Like the protected-path
// refusal it is one fixed finding keyed by ID, and every automatic resolver
// stands aside at it (HasRepeatFindingStop): a fix that did not hold once is
// diagnosed by a human, not retried by the same fixer.
const RepeatFindingFindingID = "repeat-finding"

// repeatFindingSummaryPrefix leads the stop's summary and marker description so
// an operator (and any supervisor grepping gate output) can key on it.
const repeatFindingSummaryPrefix = "repeat finding: diagnose"

// repeatFindingLadder is the guidance the stop hands the operator, in order.
const repeatFindingLadder = "Ladder after diagnosis: 1) prerequisite or brief - supply what the fix was missing or sharpen the brief; 2) change family - take a different kind of change rather than another variant of the same one; 3) stronger model - rerun the fix with a stronger model at high effort (e.g. Opus 5.5 high)."

// fixedFinding is one finding a fix round was dispatched for, and the round
// that reported it.
type fixedFinding struct {
	item  types.Finding
	round int
}

// repeatedFinding names a finding this round reported again and every round it
// appeared in, the current one last.
type repeatedFinding struct {
	id     string
	rounds []int
}

// positionalFindingID matches the IDs the pipeline assigns itself
// (NormalizeFindings' "<step>-N" and the review merge's "review-N"). Those
// name a position in one round's output, not a defect, so they never match a
// finding across rounds on their own.
var positionalFindingID = regexp.MustCompile(`^[a-z][a-z-]*-[0-9]+$`)

// fixedFindingsFromRounds rebuilds, from a step's durable rounds, every
// finding an earlier round dispatched to the fixer - by auto-fix OR by an
// operator's fix response. Reading the durable selection (the same record
// autoFixAttempts is re-derived from) is what lets the repeat stop see a fix
// dispatched before a daemon restart or through a recovered gate, and a fix a
// driving agent chose with `axi respond --action fix` while auto_fix is off.
func fixedFindingsFromRounds(rounds []*db.StepRound) []fixedFinding {
	var dispatched []fixedFinding
	for _, round := range rounds {
		if round.SelectionSource == nil || round.SelectedFindingIDs == nil {
			continue
		}
		if source := *round.SelectionSource; source != db.RoundSelectionSourceAutoFix && source != db.RoundSelectionSourceUser {
			continue
		}
		selected := make(map[string]bool)
		for _, id := range findingIDsFromSelectionJSON(*round.SelectedFindingIDs) {
			if id != RepeatFindingFindingID {
				selected[id] = true
			}
		}
		for _, raw := range []*string{round.UserFindingsJSON, round.FindingsJSON} {
			if raw == nil {
				continue
			}
			findings, err := types.ParseFindingsJSON(*raw)
			if err != nil {
				continue
			}
			for _, item := range findings.Items {
				if selected[item.ID] {
					delete(selected, item.ID)
					dispatched = append(dispatched, fixedFinding{item: item, round: round.Round})
				}
			}
		}
	}
	return dispatched
}

// sameFindingAcrossRounds is the repeat stop's identity rule. Finding IDs are
// chosen by the agent and are NOT guaranteed stable across rounds, so two
// findings are the same defect when either
//   - both carry the same agent-chosen ID (never a positional pipeline ID,
//     which names a slot in one round's output), or
//   - they name the same file with the same description after case and
//     whitespace normalization (line, severity, and action are ignored: a fix
//     moves lines, and a rereview may re-grade what it reports again).
//
// A reworded description under a fresh positional ID is deliberately not a
// repeat: the stop must never halt the fixer on a finding it cannot show was
// reported before.
func sameFindingAcrossRounds(a, b types.Finding) bool {
	if a.ID != "" && a.ID == b.ID && !positionalFindingID.MatchString(a.ID) {
		return true
	}
	desc := normalizeFindingText(a.Description)
	return desc != "" && desc == normalizeFindingText(b.Description) && normalizeCoveredPath(a.File) == normalizeCoveredPath(b.File)
}

func normalizeFindingText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// repeatedFixedFindings returns the actionable findings in raw (a round's own
// output, never the review step's carried outstanding set) that an earlier
// fix round was already dispatched for. A new finding matches nothing and so
// never triggers the stop, and neither does a no-op note or a review question,
// which a fixer is never handed.
func repeatedFixedFindings(raw string, dispatched []fixedFinding, round int) []repeatedFinding {
	if raw == "" || len(dispatched) == 0 {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	var repeats []repeatedFinding
	for _, item := range findings.Items {
		if item.Action == types.ActionNoOp || item.Category == types.FindingCategoryReviewQuestion || item.ID == RepeatFindingFindingID {
			continue
		}
		seen := map[int]bool{}
		var rounds []int
		for _, earlier := range dispatched {
			if sameFindingAcrossRounds(item, earlier.item) && !seen[earlier.round] {
				seen[earlier.round] = true
				rounds = append(rounds, earlier.round)
			}
		}
		if len(rounds) == 0 {
			continue
		}
		sort.Ints(rounds)
		repeats = append(repeats, repeatedFinding{id: item.ID, rounds: append(rounds, round)})
	}
	return repeats
}

// repeatFindingStopDescription is the stop's operator-facing text.
func repeatFindingStopDescription(repeats []repeatedFinding) string {
	parts := make([]string, 0, len(repeats))
	for _, r := range repeats {
		rounds := make([]string, 0, len(r.rounds))
		for _, n := range r.rounds {
			rounds = append(rounds, strconv.Itoa(n))
		}
		id := r.id
		if id == "" {
			id = "(unnamed finding)"
		}
		parts = append(parts, fmt.Sprintf("%s (rounds %s)", id, strings.Join(rounds, ", ")))
	}
	return fmt.Sprintf("%s - the automated fixer stopped instead of running another fix round: %s reported again after a fix round already addressed %s. Diagnose why the fix did not hold before fixing again. %s",
		repeatFindingSummaryPrefix, strings.Join(parts, "; "), pluralize(len(repeats), "it", "them"), repeatFindingLadder)
}

// withRepeatFindingStopJSON adds the stop's ask-user marker to a gate's
// findings and leads its summary with the stop, so the gate parks for a
// human instead of dispatching another fix round.
func withRepeatFindingStopJSON(raw string, repeats []repeatedFinding) string {
	var findings types.Findings
	if raw != "" {
		parsed, err := types.ParseFindingsJSON(raw)
		if err != nil {
			return raw
		}
		findings = parsed
	}
	description := repeatFindingStopDescription(repeats)
	findings.Items = append(findings.Items, types.Finding{
		ID:          RepeatFindingFindingID,
		Severity:    types.FindingSeverityError,
		Description: description,
		Action:      types.ActionAskUser,
	})
	if findings.Summary != "" {
		findings.Summary = description + "\n\n" + findings.Summary
	} else {
		findings.Summary = description
	}
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw
	}
	return encoded
}

// dropRepeatFindingStopJSON removes the stop's marker from the review step's
// carried outstanding set. The marker has no File, so carrying it would make
// hasUnanchoredFinding refuse to clear ANY selected finding for the rest of the
// step, and it is re-derived from the rounds whenever a repeat recurs.
func dropRepeatFindingStopJSON(raw string) string {
	if !hasFindingID(raw, RepeatFindingFindingID) {
		return raw
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	kept := findings.Items[:0]
	for _, item := range findings.Items {
		if item.ID != RepeatFindingFindingID {
			kept = append(kept, item)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	findings.Items = kept
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw
	}
	return encoded
}

// HasRepeatFindingStop identifies a gate the automated fixer stopped at
// because a finding it already fixed was reported again. Every automatic
// resolver reads this one predicate and stands aside: sending fix there is
// exactly the extra round the stop exists to prevent.
func HasRepeatFindingStop(findingsJSON string) bool {
	return hasFindingID(findingsJSON, RepeatFindingFindingID)
}

// repeatFixRefusal reports why a bare fix is rejected at a repeat-stop gate, or
// "" when it is accepted. Another fix round with nothing new is exactly the
// round the stop exists to prevent, so fix is accepted only with the
// diagnosis attached: per-finding instructions or an added finding (the
// ladder's prerequisite or brief). Approve, skip, and abort stay open.
func repeatFixRefusal(step types.StepName, findingsJSON string) string {
	if !HasRepeatFindingStop(findingsJSON) {
		return ""
	}
	return fmt.Sprintf("%s: %s already reported a finding again after a fix round, so another fix needs the diagnosis attached - pass --instructions (or --add-finding) with what the fix was missing, or approve, skip, or abort", repeatFindingSummaryPrefix, step)
}

// carriesFixDiagnosis reports whether a fix response brings anything the
// previous fix round did not have.
func carriesFixDiagnosis(instructions map[string]string, added []types.Finding) bool {
	if len(added) > 0 {
		return true
	}
	for _, note := range instructions {
		if strings.TrimSpace(note) != "" {
			return true
		}
	}
	return false
}

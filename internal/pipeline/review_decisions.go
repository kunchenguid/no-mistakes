package pipeline

import (
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Durable resolution of a human-declined ask-user finding (2026-11-25 pingap
// review stall, defect 1).
//
// Before this, an explicit `--ignore` of an ask-user finding was recorded (the
// round's decline complement) and echoed, but the gate re-parked on the same
// finding after every round for ever: the decline satisfied no completion
// predicate and nothing told the driver the loop had an exit. An ask-user
// finding is a question the human rules on; an explicit `--ignore` IS that
// ruling ("keep the code as-is"), so it durably removes the finding from the
// live outstanding set. It stays visible in the round history and every later
// prompt as declined (declinedFindingLines derives it from the same rounds),
// and a reviewer may still re-raise it against genuinely changed code - a
// re-report merges back in as a new item and parks for a fresh decision.
//
// Only ask-user findings are dropped this way. An auto-fix decline stays
// advisory and outstanding, exactly as before. Omission alone never declines
// (splitFixResponse refuses an unaccounted finding), so a drop can only follow
// an explicit `--ignore`; the safety rule that omission never declines is
// untouched.
//
// The decision set is derived, never stored (issue #790 decision B): a finding
// is human-declined when some human-decided round of the same step showed it
// (by content identity, so a re-minted ID does not lose the decision) and no
// human round chose it for a fix - the same sticky rule declinedFindingLines
// renders for the prompts. This mirrors splitFixResponse's own content-identity
// decision model (earlierUserDecisionIDs), so a content-identical re-raise is
// already treated as decided there.

// humanDeclinedFindingKeys returns the content identities of findings a human
// explicitly declined in an earlier round of this step and never chose to fix.
// An unreadable round payload contributes nothing: a missing decision only ever
// keeps a finding outstanding, never drops one, so this fails toward parking.
func humanDeclinedFindingKeys(rounds []*db.StepRound) map[string]bool {
	chosen := make(map[string]bool)
	seen := make(map[string]bool)
	for _, round := range rounds {
		if !humanDecidedRound(round) {
			continue
		}
		if round.FindingsJSON != nil {
			if findings, err := types.ParseFindingsJSON(*round.FindingsJSON); err == nil {
				for _, item := range findings.Items {
					seen[FindingDecisionKey(item)] = true
				}
			}
		}
		for key := range ChosenFindingKeys(round) {
			chosen[key] = true
		}
	}
	declined := make(map[string]bool)
	for key := range seen {
		if !chosen[key] {
			declined[key] = true
		}
	}
	return declined
}

// dropDeclinedAskUserFindings removes from an outstanding set every ask-user
// finding a human explicitly declined in an earlier round of the same step. It
// is applied when a fix response is dispatched (live and recovered) and when
// recovery seeds the outstanding set, so a declined ask-user finding never
// rides the live gate as work and a crashed fix round does not resurrect it.
// Auto-fix findings and undecided ask-user findings are untouched. An
// unreadable payload is returned unchanged (fail toward parking).
func dropDeclinedAskUserFindings(raw string, rounds []*db.StepRound) string {
	if raw == "" {
		return raw
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	declined := humanDeclinedFindingKeys(rounds)
	if len(declined) == 0 {
		return raw
	}
	result := types.FindingsMetadata(findings)
	for _, item := range findings.Items {
		if item.ActionOrDefault() == types.ActionAskUser && declined[FindingDecisionKey(item)] {
			continue
		}
		result.Items = append(result.Items, item)
	}
	if len(result.Items) == len(findings.Items) {
		return raw
	}
	if len(result.Items) == 0 {
		return ""
	}
	encoded, err := types.MarshalFindingsJSON(result)
	if err != nil {
		return raw
	}
	return encoded
}

// HasDeclinedAskUserFinding reports whether a gate still carries an ask-user
// finding a human explicitly declined. It is observability for the gate help
// and the `recorded:` echo; the durable drop means such a finding normally no
// longer appears at a live gate at all.
func HasDeclinedAskUserFinding(gate string, rounds []*db.StepRound) bool {
	findings, err := types.ParseFindingsJSON(gate)
	if err != nil {
		return false
	}
	declined := humanDeclinedFindingKeys(rounds)
	for _, item := range findings.Items {
		if item.ActionOrDefault() == types.ActionAskUser && declined[FindingDecisionKey(item)] {
			return true
		}
	}
	return false
}

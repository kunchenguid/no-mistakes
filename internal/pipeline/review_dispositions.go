package pipeline

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// settleableReviewFinding reports whether a human finding disposition can
// settle this finding at all. A reviewer's open question is resolved by its
// answer and nothing else, and a reserved finding is pipeline-owned and
// re-derived on every turn, so neither is ever suppressed by an ignore or an
// external disposition - the same carve-out dropReviewQuestionFindingsJSON
// applies to the append-only carry-forward.
func settleableReviewFinding(item types.Finding) bool {
	return item.Category != types.FindingCategoryReviewQuestion && !IsReservedFindingID(item.ID)
}

// filterSettledReviewFindings keeps a recorded human disposition effective when
// a later reviewer re-mints an ID or moves a finding's line. A fingerprint is
// accepted only when it identifies one finding in each report; ambiguous
// matches fail closed. Approve/skip/abort do not settle individual findings.
//
// A finding any human round of this step chose to fix is never settled: the
// gate refuses a decline or an external disposition that names it, and an
// omission the gate permitted because an earlier round already decided it is
// not a decline. settledReviewDispositions withholds the credit, so a moved
// line and an ambiguous gate cannot carry a fix selection out of the gate.
func filterSettledReviewFindings(raw string, rounds []*db.StepRound) (string, error) {
	if raw == "" || len(rounds) == 0 {
		return raw, nil
	}
	current, err := types.ParseFindingsJSON(raw)
	if err != nil {
		// The ordinary gate still handles an unreadable review report. Do not
		// turn its refusal into a failed run merely because rounds exist.
		return raw, nil
	}
	// Legacy and test reports without a pinned trusted scope cannot gain
	// certification by dropping findings from the gate.
	if current.ReviewCoverageScope == nil {
		return raw, nil
	}
	ignored, external, err := settledReviewDispositions(rounds)
	if err != nil {
		return "", err
	}
	if len(ignored) == 0 && len(external) == 0 {
		return raw, nil
	}
	counts := countFindingFingerprints(current.Items)
	filtered := types.FindingsMetadata(current)
	removed := false
	filtered.Items = make([]types.Finding, 0, len(current.Items))
	for _, item := range current.Items {
		key := findingFingerprint(item)
		if settleableReviewFinding(item) && counts[key] == 1 && (ignored[key] || external[key]) {
			removed = true
			continue
		}
		filtered.Items = append(filtered.Items, item)
	}
	if !removed {
		return raw, nil
	}
	return types.MarshalFindingsJSON(filtered)
}

// SettledReviewFindingCount reports how many distinct findings a human
// disposition settled without a fix across this step's rounds. It reads the
// same two sets filterSettledReviewFindings suppresses from, so a finding the
// operator chose to fix is never published as settled. Unreadable rounds count
// nothing: the filter fails the step on them, so a completed step's rounds
// always parse.
func SettledReviewFindingCount(rounds []*db.StepRound) int {
	ignored, external, err := settledReviewDispositions(rounds)
	if err != nil {
		return 0
	}
	settled := make(map[types.Finding]bool, len(ignored)+len(external))
	for key := range ignored {
		settled[key] = true
	}
	for key := range external {
		settled[key] = true
	}
	return len(settled)
}

// settledReviewDispositions reads this step's recorded human dispositions into
// the two fingerprint sets every consumer decides on: the findings an ignore
// declined and the findings settled outside the worktree. Neither set ever
// contains a finding a human round of this step chose to fix. A fix response
// may omit a finding an earlier round decided, and that omission rules on
// nothing, which is the same subtraction the reviewer's round history performs.
// A disposition is credited only for a fingerprint that identifies one finding
// in its own round's payload, because an ambiguous one could never be matched
// again; the fix selection carries no such condition, so the protection stays
// wider than the suppression.
func settledReviewDispositions(rounds []*db.StepRound) (ignored, external map[types.Finding]bool, err error) {
	ignored = make(map[types.Finding]bool)
	external = make(map[types.Finding]bool)
	chosen := chosenToFixFingerprints(rounds)
	for _, round := range rounds {
		if round == nil || round.FindingsJSON == nil || round.SelectionSource == nil {
			continue
		}
		source := *round.SelectionSource
		if source != db.RoundSelectionSourceUser && source != db.RoundSelectionSourceUserIgnored && source != db.RoundSelectionSourceExternal {
			continue
		}
		shown, err := types.ParseFindingsJSON(*round.FindingsJSON)
		if err != nil {
			return nil, nil, fmt.Errorf("read review disposition: %w", err)
		}
		if round.SelectedFindingIDs == nil {
			return nil, nil, fmt.Errorf("review disposition is missing its selection")
		}
		var selected []string
		if err := json.Unmarshal([]byte(*round.SelectedFindingIDs), &selected); err != nil {
			return nil, nil, fmt.Errorf("read review selection: %w", err)
		}
		shownCounts := countFindingFingerprints(shown.Items)
		if source == db.RoundSelectionSourceExternal {
			if round.UserFindingsJSON == nil || len(selected) != 0 {
				return nil, nil, fmt.Errorf("external review disposition is incomplete")
			}
			resolved, err := types.ParseFindingsJSON(*round.UserFindingsJSON)
			if err != nil {
				return nil, nil, fmt.Errorf("read external review disposition: %w", err)
			}
			for _, item := range resolved.Items {
				if item.UserInstructions == "" {
					return nil, nil, fmt.Errorf("external review disposition is missing its reason")
				}
				for _, original := range shown.Items {
					if !settleableReviewFinding(original) {
						continue
					}
					key := findingFingerprint(original)
					if original.ID == item.ID && SameFinding(original, item) && shownCounts[key] == 1 && !chosen[key] {
						external[key] = true
					}
				}
			}
			continue
		}
		for _, item := range shown.Items {
			if !settleableReviewFinding(item) {
				continue
			}
			key := findingFingerprint(item)
			if shownCounts[key] != 1 || chosen[key] {
				continue
			}
			ignored[key] = true
		}
	}
	return ignored, external, nil
}

// ambiguousSettlementIDs names the findings whose disposition could never be
// matched again, because another finding in the same payload shares their
// fingerprint. filterSettledReviewFindings skips exactly those, so recording
// one would report a decision the run can never act on.
func ambiguousSettlementIDs(gateFindingsJSON string, ids []string) []string {
	if len(ids) == 0 || gateFindingsJSON == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(gateFindingsJSON)
	if err != nil {
		return nil
	}
	named := make(map[string]bool, len(ids))
	for _, id := range ids {
		named[id] = true
	}
	counts := countFindingFingerprints(findings.Items)
	var ambiguous []string
	seen := make(map[string]bool, len(ids))
	for _, item := range findings.Items {
		if !named[item.ID] || seen[item.ID] || !settleableReviewFinding(item) {
			continue
		}
		if counts[findingFingerprint(item)] != 1 {
			seen[item.ID] = true
			ambiguous = append(ambiguous, item.ID)
		}
	}
	return ambiguous
}

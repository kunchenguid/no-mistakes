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

// settlementIdentities records the finding instances a human decision named.
// A decision is matched again by its exact instance, and by content alone only
// when that content identified one finding in the payload the decision was
// taken from: findings differing only in line are decided one instance at a
// time, and a moved one is reported again rather than silently settled.
type settlementIdentities struct {
	exact    map[types.Finding]bool
	unique   map[types.Finding]int
	anyPlace map[types.Finding]bool
}

func newSettlementIdentities() *settlementIdentities {
	return &settlementIdentities{
		exact:    map[types.Finding]bool{},
		unique:   map[types.Finding]int{},
		anyPlace: map[types.Finding]bool{},
	}
}

func (s *settlementIdentities) record(item types.Finding, uniqueInItsPayload bool) {
	key := findingKey(item)
	if s.exact[key] {
		return
	}
	s.exact[key] = true
	fingerprint := findingFingerprint(item)
	s.anyPlace[fingerprint] = true
	if uniqueInItsPayload {
		s.unique[fingerprint]++
	}
}

// settled reports whether item is the finding a recorded decision named. It is
// the repository's ordinary finding match: content alone identifies a finding
// only when it identifies exactly one on each side.
func (s *settlementIdentities) settled(item types.Finding, counts map[types.Finding]int) bool {
	return hasFindingMatch(item, s.exact, counts, s.unique)
}

// chose reports whether item may be a finding a recorded fix selection named.
// It is deliberately wider than settled - any recorded instance of the content
// answers it - because a fix selection leaving the gate unnoticed loses the
// operator's work, while a disposition withheld only asks them again.
func (s *settlementIdentities) chose(item types.Finding, counts map[types.Finding]int) bool {
	fingerprint := findingFingerprint(item)
	return s.exact[findingKey(item)] || counts[fingerprint] == 1 && s.anyPlace[fingerprint]
}

func (s *settlementIdentities) size() int { return len(s.exact) }

// filterSettledReviewFindings keeps a recorded human disposition effective when
// a later reviewer re-mints an ID or moves a finding's line. Content identifies
// a finding only when it identifies one finding on each side; ambiguous matches
// fail closed. Approve/skip/abort do not settle individual findings.
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
	if ignored.size() == 0 && external.size() == 0 {
		return raw, nil
	}
	counts := countFindingFingerprints(current.Items)
	keyCounts := countFindingKeys(current.Items)
	filtered := types.FindingsMetadata(current)
	removed := false
	filtered.Items = make([]types.Finding, 0, len(current.Items))
	for _, item := range current.Items {
		settled := ignored.settled(item, counts) || external.settled(item, counts)
		if settleableReviewFinding(item) && keyCounts[findingKey(item)] == 1 && settled {
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
	settled := make(map[types.Finding]bool, ignored.size()+external.size())
	for key := range ignored.exact {
		settled[key] = true
	}
	for key := range external.exact {
		settled[key] = true
	}
	return len(settled)
}

// settledReviewDispositions reads this step's recorded human dispositions into
// the two identity sets every consumer decides on: the findings an ignore
// declined and the findings settled outside the worktree. Neither set ever
// contains a finding a human round of this step chose to fix. A fix response
// may omit a finding an earlier round decided, and that omission rules on
// nothing, which is the same subtraction the reviewer's round history performs.
// A disposition is credited only for a finding its own round's payload showed
// once under one identity, because two indistinguishable records cannot be
// decided apart.
func settledReviewDispositions(rounds []*db.StepRound) (ignored, external *settlementIdentities, err error) {
	ignored = newSettlementIdentities()
	external = newSettlementIdentities()
	chosen := chosenToFixSettlements(rounds)
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
		counts := countFindingFingerprints(shown.Items)
		keyCounts := countFindingKeys(shown.Items)
		settleable := func(item types.Finding) bool {
			return settleableReviewFinding(item) && keyCounts[findingKey(item)] == 1 && !chosen.chose(item, counts)
		}
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
					if original.ID == item.ID && SameFinding(original, item) && settleable(original) {
						external.record(original, counts[findingFingerprint(original)] == 1)
					}
				}
			}
			continue
		}
		for _, item := range shown.Items {
			if settleable(item) {
				ignored.record(item, counts[findingFingerprint(item)] == 1)
			}
		}
	}
	return ignored, external, nil
}

// chosenToFixSettlements reports every finding instance a human round of this
// step chose to fix, so no consumer can settle one.
func chosenToFixSettlements(rounds []*db.StepRound) *settlementIdentities {
	chosen := newSettlementIdentities()
	for _, round := range rounds {
		if !humanDecidedRound(round) {
			continue
		}
		for _, item := range selectedFindingIdentities([]*db.StepRound{round}) {
			chosen.record(item, false)
		}
	}
	return chosen
}

// ambiguousSettlementIDs names the findings whose disposition could never be
// matched again, because another finding in the same payload is an identical
// record. filterSettledReviewFindings skips exactly those, so recording one
// would report a decision the run can never act on. Two findings that differ
// only in line are not ambiguous: each is decided as its own instance.
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
	counts := countFindingKeys(findings.Items)
	var ambiguous []string
	seen := make(map[string]bool, len(ids))
	for _, item := range findings.Items {
		if !named[item.ID] || seen[item.ID] || !settleableReviewFinding(item) {
			continue
		}
		if counts[findingKey(item)] != 1 {
			seen[item.ID] = true
			ambiguous = append(ambiguous, item.ID)
		}
	}
	return ambiguous
}

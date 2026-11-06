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
	exact  map[types.Finding]bool
	unique map[types.Finding]int
}

func newSettlementIdentities() *settlementIdentities {
	return &settlementIdentities{
		exact:  map[types.Finding]bool{},
		unique: map[types.Finding]int{},
	}
}

// record takes one round's decisions. Each item is recorded under its exact
// instance, and additionally as content-matchable when that content identified
// one finding in the payload the decision was taken from.
func (s *settlementIdentities) record(items []types.Finding, counts map[types.Finding]int) {
	for _, item := range items {
		if s.exact[findingKey(item)] {
			continue
		}
		s.exact[findingKey(item)] = true
		fingerprint := findingFingerprint(item)
		if counts[fingerprint] == 1 {
			s.unique[fingerprint]++
		}
	}
}

// contents reports the distinct finding contents this set recorded, so one
// content decided at several lines or in several rounds counts once.
func (s *settlementIdentities) contents() map[types.Finding]bool {
	out := make(map[types.Finding]bool, len(s.exact))
	for key := range s.exact {
		out[findingFingerprint(key)] = true
	}
	return out
}

// matches reports whether item is the finding a recorded decision named. It is
// the repository's ordinary finding match: content alone identifies a finding
// only when it identifies exactly one on each side, so an instance a decision
// never named is reported again instead of answered for the operator.
func (s *settlementIdentities) matches(item types.Finding, counts map[types.Finding]int) bool {
	return hasFindingMatch(item, s.exact, counts, s.unique)
}

// saturate makes the content identity monotone, which a protection set needs
// and a disposition set must not have: hasFindingMatch requires the candidate
// side to hold exactly one instance of a content, so two recorded decisions
// about one content stop it matching. A disposition must then be asked again,
// but a fix selection must not stop being protected because the operator
// decided the same content twice.
func (s *settlementIdentities) saturate() {
	for fingerprint := range s.unique {
		s.unique[fingerprint] = 1
	}
}

func (s *settlementIdentities) empty() bool { return len(s.exact) == 0 }

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
	if ignored.empty() && external.empty() {
		return raw, nil
	}
	counts := countFindingFingerprints(current.Items)
	keyCounts := countFindingKeys(current.Items)
	filtered := types.FindingsMetadata(current)
	removed := false
	filtered.Items = make([]types.Finding, 0, len(current.Items))
	for _, item := range current.Items {
		settled := ignored.matches(item, counts) || external.matches(item, counts)
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

// SettledReviewFindingCount reports how many distinct finding contents a human
// disposition settled without a fix across this step's rounds. It reads the
// same two sets filterSettledReviewFindings suppresses from, so a finding the
// operator chose to fix is never published as settled. Unreadable rounds count
// nothing: the filter fails the step on them, so a completed step's rounds
// always parse.
//
// The count is a lower bound: one content settled at two lines, or settled
// again after the reviewer moved it, counts once, which is why the published
// wording reads "at least". Its consumers need no more - the forced verdict
// asks only whether anything was settled.
func SettledReviewFindingCount(rounds []*db.StepRound) int {
	ignored, external, err := settledReviewDispositions(rounds)
	if err != nil {
		return 0
	}
	settled := ignored.contents()
	for fingerprint := range external.contents() {
		settled[fingerprint] = true
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
			return settleableReviewFinding(item) && keyCounts[findingKey(item)] == 1 && !chosen.matches(item, counts)
		}
		if source == db.RoundSelectionSourceExternal {
			if round.UserFindingsJSON == nil || len(selected) != 0 {
				return nil, nil, fmt.Errorf("external review disposition is incomplete")
			}
			resolved, err := types.ParseFindingsJSON(*round.UserFindingsJSON)
			if err != nil {
				return nil, nil, fmt.Errorf("read external review disposition: %w", err)
			}
			var settled []types.Finding
			for _, item := range resolved.Items {
				if item.UserInstructions == "" {
					return nil, nil, fmt.Errorf("external review disposition is missing its reason")
				}
				for _, original := range shown.Items {
					if original.ID == item.ID && SameFinding(original, item) && settleable(original) {
						settled = append(settled, original)
					}
				}
			}
			external.record(settled, counts)
			continue
		}
		var declined []types.Finding
		for _, item := range shown.Items {
			if settleable(item) {
				declined = append(declined, item)
			}
		}
		ignored.record(declined, counts)
	}
	return ignored, external, nil
}

// chosenToFixSettlements reports every finding instance a human round of this
// step chose to fix, so no consumer can settle one. A selection taken from a
// payload that showed its content twice speaks only for the instance it named:
// claiming the other one too refused the operator's own decline of a finding
// they never selected. Repeated selections of one content never weaken that
// protection, which is what saturate holds.
func chosenToFixSettlements(rounds []*db.StepRound) *settlementIdentities {
	chosen := newSettlementIdentities()
	for _, round := range rounds {
		if !humanDecidedRound(round) {
			continue
		}
		var shown []types.Finding
		if round.FindingsJSON != nil {
			if payload, err := types.ParseFindingsJSON(*round.FindingsJSON); err == nil {
				shown = payload.Items
			}
		}
		gate := countFindingFingerprints(shown)
		var selected []types.Finding
		for _, item := range selectedFindingIdentities([]*db.StepRound{round}) {
			selected = append(selected, item)
			if gate[findingFingerprint(item)] == 0 {
				shown = append(shown, item)
			}
		}
		chosen.record(selected, countFindingFingerprints(shown))
	}
	chosen.saturate()
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

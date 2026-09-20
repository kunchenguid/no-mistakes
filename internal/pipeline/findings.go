package pipeline

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// findingIDsJSON extracts the finding IDs from a findings JSON payload and
// returns them as a JSON array string. Empty result means there were no
// findings or parsing failed.
func findingIDsJSON(raw string) string {
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return ""
	}
	ids := make([]string, 0, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID == "" {
			continue
		}
		ids = append(ids, item.ID)
	}
	return marshalFindingIDs(ids)
}

// findingIDList extracts the finding IDs from a findings JSON payload as a
// plain slice (no JSON encoding), for selection bookkeeping like the review
// loop's pending-verification set.
func findingIDList(raw string) []string {
	if raw == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// marshalFindingIDs encodes a list of finding IDs as a JSON array. Empty
// input returns an empty string so the caller can leave the DB column NULL.
func marshalFindingIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func findingKey(item types.Finding) types.Finding {
	item.ID = ""
	item.Action = ""
	item.Source = ""
	item.UserInstructions = ""
	return item
}

func findingFingerprint(item types.Finding) types.Finding {
	item = findingKey(item)
	item.Line = 0
	return item
}

func countFindingFingerprints(items []types.Finding) map[types.Finding]int {
	counts := make(map[types.Finding]int, len(items))
	for _, item := range items {
		counts[findingFingerprint(item)]++
	}
	return counts
}

func hasFindingMatch(item types.Finding, exact map[types.Finding]bool, itemCounts, candidateCounts map[types.Finding]int) bool {
	if exact[findingKey(item)] {
		return true
	}
	fingerprint := findingFingerprint(item)
	return itemCounts[fingerprint] == 1 && candidateCounts[fingerprint] == 1
}

func normalizeFindingsJSON(raw string, prefix string) string {
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	normalized := types.NormalizeFindings(findings, prefix)
	normalizedRaw, err := types.MarshalFindingsJSON(normalized)
	if err != nil {
		return raw
	}
	return normalizedRaw
}

func excludeFindingsJSON(raw string, ids []string) string {
	if raw == "" || len(ids) == 0 {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return ""
	}
	excluded := types.ExcludeFindings(findings, ids)
	if len(excluded.Items) == 0 {
		return ""
	}
	excludedRaw, err := types.MarshalFindingsJSON(excluded)
	if err != nil {
		return ""
	}
	return excludedRaw
}

func mergeFindingsJSON(existingRaw, additionalRaw string) string {
	if existingRaw == "" {
		return additionalRaw
	}
	if additionalRaw == "" {
		return existingRaw
	}
	existing, err := types.ParseFindingsJSON(existingRaw)
	if err != nil {
		return additionalRaw
	}
	additional, err := types.ParseFindingsJSON(additionalRaw)
	if err != nil {
		return existingRaw
	}
	seen := make(map[types.Finding]bool, len(existing.Items)+len(additional.Items))
	existingCounts := countFindingFingerprints(existing.Items)
	additionalCounts := countFindingFingerprints(additional.Items)
	merged := types.FindingsMetadata(existing)
	for _, item := range existing.Items {
		merged.Items = append(merged.Items, item)
		seen[findingKey(item)] = true
	}
	for _, item := range additional.Items {
		if hasFindingMatch(item, seen, additionalCounts, existingCounts) {
			continue
		}
		key := findingKey(item)
		if seen[key] {
			continue
		}
		merged.Items = append(merged.Items, item)
		seen[key] = true
	}
	if len(merged.Items) == 0 {
		return ""
	}
	mergedRaw, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return existingRaw
	}
	return mergedRaw
}

func removeMatchingFindingsJSON(existingRaw, removeRaw string) string {
	if existingRaw == "" || removeRaw == "" {
		return existingRaw
	}
	existing, err := types.ParseFindingsJSON(existingRaw)
	if err != nil {
		return existingRaw
	}
	remove, err := types.ParseFindingsJSON(removeRaw)
	if err != nil {
		return existingRaw
	}
	toRemove := make(map[types.Finding]bool, len(remove.Items))
	existingCounts := countFindingFingerprints(existing.Items)
	removeCounts := countFindingFingerprints(remove.Items)
	for _, item := range remove.Items {
		toRemove[findingKey(item)] = true
	}
	filtered := types.FindingsMetadata(existing)
	for _, item := range existing.Items {
		if hasFindingMatch(item, toRemove, existingCounts, removeCounts) {
			continue
		}
		filtered.Items = append(filtered.Items, item)
	}
	if len(filtered.Items) == 0 {
		return ""
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return existingRaw
	}
	return filteredRaw
}

func retainMatchingFindingsJSON(existingRaw, keepRaw string) string {
	if existingRaw == "" || keepRaw == "" {
		return ""
	}
	existing, err := types.ParseFindingsJSON(existingRaw)
	if err != nil {
		return ""
	}
	keep, err := types.ParseFindingsJSON(keepRaw)
	if err != nil {
		return ""
	}
	allowed := make(map[types.Finding]bool, len(keep.Items))
	existingCounts := countFindingFingerprints(existing.Items)
	keepCounts := countFindingFingerprints(keep.Items)
	for _, item := range keep.Items {
		allowed[findingKey(item)] = true
	}
	filtered := types.FindingsMetadata(existing)
	for _, item := range existing.Items {
		if !hasFindingMatch(item, allowed, existingCounts, keepCounts) {
			continue
		}
		filtered.Items = append(filtered.Items, item)
	}
	if len(filtered.Items) == 0 {
		return ""
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return ""
	}
	return filteredRaw
}

func autoFixableFindingsJSON(raw string) string {
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	fixable := types.AutoFixableFindings(findings)
	if len(fixable.Items) == 0 {
		return ""
	}
	fixableRaw, err := types.MarshalFindingsJSON(fixable)
	if err != nil {
		return raw
	}
	return fixableRaw
}

func hasAskUserFindingsJSON(raw string) bool {
	if raw == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	return types.HasAskUserFindings(findings)
}

func hasBlockingFindingsJSON(raw string) bool {
	if raw == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return true
	}
	for _, item := range findings.Items {
		if item.Severity == types.FindingSeverityError || item.Severity == types.FindingSeverityWarning {
			return true
		}
	}
	return false
}

func findingIDsFromSelectionJSON(raw string) []string {
	if raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}

func combineFindingIDLists(existing, additional []string) []string {
	result := append([]string(nil), existing...)
	seen := make(map[string]bool, len(result))
	for _, id := range result {
		if id != "" {
			seen[id] = true
		}
	}
	for _, id := range additional {
		if id != "" && !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result
}

func retainFindingIDs(raw string, ids []string) []string {
	if len(ids) == 0 || raw == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return append([]string(nil), ids...)
	}
	present := make(map[string]bool, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID != "" {
			present[item.ID] = true
		}
	}
	retained := make([]string, 0, len(ids))
	for _, id := range ids {
		if present[id] {
			retained = append(retained, id)
		}
	}
	return retained
}

// selectedFindingIdentities records, for every finding ID a round's
// SelectedFindingIDs named, the content identity (findingKey) that ID
// referred to as of that round's FindingsJSON, or its UserFindingsJSON when
// the ID was not present in the primary findings. Finding IDs are positional
// and get re-minted for an unrelated finding once the original is resolved
// and drops out of the outstanding set. A later round's selection under the
// same ID legitimately overwrites an earlier entry here, since that is the
// operator selecting a new finding that happens to have inherited a freed ID.
func selectedFindingIdentities(rounds []*db.StepRound) map[string]types.Finding {
	identity := make(map[string]types.Finding)
	for _, round := range rounds {
		if round.SelectedFindingIDs == nil {
			continue
		}
		ids := findingIDsFromSelectionJSON(*round.SelectedFindingIDs)
		if len(ids) == 0 {
			continue
		}

		byID := make(map[string]types.Finding)
		if round.FindingsJSON != nil {
			if findings, err := types.ParseFindingsJSON(*round.FindingsJSON); err == nil {
				for _, item := range findings.Items {
					if item.ID != "" {
						byID[item.ID] = item
					}
				}
			}
		}
		if round.UserFindingsJSON != nil {
			if findings, err := types.ParseFindingsJSON(*round.UserFindingsJSON); err == nil {
				for _, item := range findings.Items {
					if item.ID != "" {
						if _, exists := byID[item.ID]; !exists {
							byID[item.ID] = item
						}
					}
				}
			}
		}
		for _, id := range ids {
			if item, ok := byID[id]; ok {
				identity[id] = item
			}
		}
	}
	return identity
}

// retainFindingIDsByIdentity is retainFindingIDs plus a content-identity
// check: an ID only survives into the retained set when the finding it
// currently names in latestRaw has the SAME content (findingKey) as the
// finding selectedFindingIdentities recorded for it. An ID with no recorded
// identity is not retained because recovery cannot prove that it still names
// the finding the operator selected.
func retainFindingIDsByIdentity(latestRaw string, ids []string, identity map[string]types.Finding) []string {
	if len(ids) == 0 || latestRaw == "" {
		return nil
	}
	latest, err := types.ParseFindingsJSON(latestRaw)
	if err != nil {
		return nil
	}
	byID := make(map[string]types.Finding, len(latest.Items))
	for _, item := range latest.Items {
		if item.ID != "" {
			byID[item.ID] = item
		}
	}
	retained := make([]string, 0, len(ids))
	for _, id := range ids {
		current, ok := byID[id]
		if !ok {
			continue
		}
		want, known := identity[id]
		if !known || findingKey(current) != findingKey(want) {
			continue
		}
		retained = append(retained, id)
	}
	return retained
}

func hasSelectedFindingsJSON(raw string, ids []string) bool {
	if len(ids) == 0 || raw == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return true
	}
	present := make(map[string]bool, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID != "" {
			present[item.ID] = true
		}
	}
	for _, id := range ids {
		if present[id] {
			return true
		}
	}
	return false
}

func remapFindingIDsJSON(mergedRaw, selectedRaw string) string {
	if mergedRaw == "" || selectedRaw == "" {
		return selectedRaw
	}
	merged, err := types.ParseFindingsJSON(mergedRaw)
	if err != nil {
		return selectedRaw
	}
	selected, err := types.ParseFindingsJSON(selectedRaw)
	if err != nil {
		return selectedRaw
	}
	mergedCounts := countFindingFingerprints(merged.Items)
	selectedCounts := countFindingFingerprints(selected.Items)
	used := make(map[int]bool, len(selected.Items))
	for i := range selected.Items {
		matched := false
		for j := range merged.Items {
			if used[j] || findingKey(selected.Items[i]) != findingKey(merged.Items[j]) {
				continue
			}
			selected.Items[i].ID = merged.Items[j].ID
			used[j] = true
			matched = true
			break
		}
		if matched {
			continue
		}
		fingerprint := findingFingerprint(selected.Items[i])
		if selectedCounts[fingerprint] != 1 || mergedCounts[fingerprint] != 1 {
			continue
		}
		for j := range merged.Items {
			if !used[j] && findingFingerprint(merged.Items[j]) == fingerprint {
				selected.Items[i].ID = merged.Items[j].ID
				used[j] = true
				break
			}
		}
	}
	remapped, err := types.MarshalFindingsJSON(selected)
	if err != nil {
		return selectedRaw
	}
	return remapped
}

// normalizeCoveredPath canonicalizes a reviewed or finding path for coverage
// comparison. A mismatch (including a finding with no file at all) fails the
// verification closed: the item simply stays outstanding.
func normalizeCoveredPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// resolveVerifiedFindingsJSON returns outstandingRaw minus every finding whose
// ID is in pendingIDs and for which this round is a POSITIVE verification
// record. An ordinary finding still requires trusted ReviewedPaths coverage
// of its file and no current finding in that file. A synthesized recorded-
// decision finding instead requires exactly one current satisfied assessment
// for its durable decision identity, with nonblank evidence; file coverage
// cannot express verification for ignored, absent, or file-less decisions.
//
// This is the only way a selected-and-fixed finding leaves the outstanding set
// besides an explicit operator action (approve/skip/abort). A file the round
// did not list, a missing coverage record, a finding with no file, a round
// that re-reports the defect, or a round that reports ANY OTHER finding in the
// same file all leave the item in place. Any file-less finding in the current
// round also blocks verification of every selected file-anchored item in that
// round: silence, or a round that did not look, is never resolution, and
// neither is an ambiguous report that might be the same defect shifted to
// another line or reworded. Without this last check, a fix that moves a defect
// within the same file and a rereview that describes it differently would both
// fail the exact-match and content-match checks, so the defect would silently
// clear as "not reported" even though it is still present, just relocated or
// restated. That is the P1 this closes - the predecessor dropped a selected
// finding the moment its fix was requested, so a no-op fix could let the run
// complete with the defect unresolved.
func resolveVerifiedFindingsJSON(outstandingRaw string, pendingIDs []string, reviewedPaths, reviewablePaths []string, thisRoundRaw string) string {
	if outstandingRaw == "" || len(pendingIDs) == 0 {
		return outstandingRaw
	}
	outstanding, err := types.ParseFindingsJSON(outstandingRaw)
	if err != nil {
		return outstandingRaw
	}
	pending := make(map[string]bool, len(pendingIDs))
	for _, id := range pendingIDs {
		if id != "" {
			pending[id] = true
		}
	}
	if len(pending) == 0 {
		return outstandingRaw
	}
	reviewable := make(map[string]bool, len(reviewablePaths))
	for _, candidate := range reviewablePaths {
		if normalized := normalizeCoveredPath(candidate); normalized != "" {
			reviewable[normalized] = true
		}
	}
	coverageValid := len(reviewable) > 0 && len(reviewedPaths) > 0
	covered := make(map[string]bool, len(reviewedPaths))
	for _, reviewed := range reviewedPaths {
		normalized := normalizeCoveredPath(reviewed)
		if normalized == "" || !reviewable[normalized] {
			coverageValid = false
			continue
		}
		covered[normalized] = true
	}
	if len(covered) == 0 {
		coverageValid = false
	}
	thisRound, _ := types.ParseFindingsJSON(thisRoundRaw)
	decisionReviews := make(map[string][]types.DecisionReview, len(thisRound.DecisionReviews))
	for _, review := range thisRound.DecisionReviews {
		decisionReviews[review.DecisionID] = append(decisionReviews[review.DecisionID], review)
	}
	reported := make(map[types.Finding]bool, len(thisRound.Items))
	reportedFiles := make(map[string]bool, len(thisRound.Items))
	reportedDecisionIDs := make(map[string]bool, len(thisRound.Items))
	hasUnanchoredFinding := false
	for _, item := range thisRound.Items {
		reported[findingKey(item)] = true
		if item.DecisionID != "" {
			reportedDecisionIDs[item.DecisionID] = true
		}
		if normalized := normalizeCoveredPath(item.File); normalized != "" {
			reportedFiles[normalized] = true
		} else {
			hasUnanchoredFinding = true
		}
	}
	outstandingCounts := countFindingFingerprints(outstanding.Items)
	thisRoundCounts := countFindingFingerprints(thisRound.Items)
	decisionFindingCounts := make(map[string]int)
	for _, item := range outstanding.Items {
		if item.DecisionID != "" {
			decisionFindingCounts[item.DecisionID]++
		}
	}
	result := types.FindingsMetadata(outstanding)
	for _, item := range outstanding.Items {
		if pending[item.ID] && item.DecisionID != "" {
			reviews := decisionReviews[item.DecisionID]
			if decisionFindingCounts[item.DecisionID] == 1 && len(reviews) == 1 && reviews[0].Result == "satisfied" && strings.TrimSpace(reviews[0].Evidence) != "" && !reportedDecisionIDs[item.DecisionID] {
				continue
			}
			result.Items = append(result.Items, item)
			continue
		}
		file := normalizeCoveredPath(item.File)
		if pending[item.ID] && coverageValid && !hasUnanchoredFinding && covered[file] && !hasFindingMatch(item, reported, outstandingCounts, thisRoundCounts) && !reportedFiles[file] {
			continue
		}
		result.Items = append(result.Items, item)
	}
	if len(result.Items) == len(outstanding.Items) {
		return outstandingRaw
	}
	if len(result.Items) == 0 {
		return ""
	}
	encoded, err := types.MarshalFindingsJSON(result)
	if err != nil {
		return outstandingRaw
	}
	return encoded
}

// mergeOutstandingFindingsJSON merges one review round's output into the
// append-only outstanding set.
//
// Two things make it more than mergeFindingsJSON: the merged set keeps only
// one item per ID (this round's positional normalization can re-mint an ID an
// outstanding item already holds, and the outstanding item's ID is what
// `axi respond --findings <id>` selects, so the colliding NEW item is
// re-minted instead), and the merged payload carries this round's coverage
// and decision-assessment records rather than the outstanding set's stale
// copies.
//
// Identity is still content-derived: a reworded restatement of an existing
// finding does not match its original and is appended as a second item. That
// is accepted rather than fixed here - it over-blocks instead of dropping
// anything, and stable finding identity is a separate design pass (Parts 2+3
// of the scout report).
func mergeOutstandingFindingsJSON(existingRaw, additionalRaw string, reviewedPaths []string) string {
	var currentDecisionReviews []types.DecisionReview
	if current, err := types.ParseFindingsJSON(additionalRaw); err == nil {
		currentDecisionReviews = append([]types.DecisionReview(nil), current.DecisionReviews...)
	}
	if additionalRaw == "" {
		if existingRaw == "" {
			return ""
		}
		findings, err := types.ParseFindingsJSON(existingRaw)
		if err != nil {
			return existingRaw
		}
		findings.ReviewedPaths = append([]string(nil), reviewedPaths...)
		findings.DecisionReviews = currentDecisionReviews
		encoded, err := types.MarshalFindingsJSON(findings)
		if err != nil {
			return existingRaw
		}
		return encoded
	}
	mergedRaw := mergeFindingsJSON(existingRaw, additionalRaw)
	if mergedRaw == "" {
		return ""
	}
	merged, err := types.ParseFindingsJSON(mergedRaw)
	if err != nil {
		return mergedRaw
	}
	merged.ReviewedPaths = append([]string(nil), reviewedPaths...)
	merged.DecisionReviews = currentDecisionReviews
	seenDecisionIDs := make(map[string]bool)
	items := merged.Items[:0]
	for _, item := range merged.Items {
		if item.DecisionID != "" {
			if seenDecisionIDs[item.DecisionID] {
				continue
			}
			seenDecisionIDs[item.DecisionID] = true
		}
		items = append(items, item)
	}
	merged.Items = items
	seen := make(map[string]bool, len(merged.Items))
	for i := range merged.Items {
		id := merged.Items[i].ID
		if id != "" && !seen[id] {
			seen[id] = true
			continue
		}
		merged.Items[i].ID = nextFreeReviewFindingID(seen)
		seen[merged.Items[i].ID] = true
	}
	encoded, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return mergedRaw
	}
	return encoded
}

func nextFreeReviewFindingID(seen map[string]bool) string {
	for i := 1; ; i++ {
		id := "review-" + strconv.Itoa(i)
		if !seen[id] {
			return id
		}
	}
}

// combineSelectedFindingIDs returns the ordered list of finding IDs that
// were dispatched to the fix agent: the user's selected agent-produced
// IDs plus any user-authored finding IDs (which only appear in the merged
// list).
func combineSelectedFindingIDs(selected []string, mergedFindings string) []string {
	if mergedFindings == "" {
		return selected
	}
	merged, err := types.ParseFindingsJSON(mergedFindings)
	if err != nil {
		return selected
	}
	seen := make(map[string]bool, len(selected))
	for _, id := range selected {
		if id != "" {
			seen[id] = true
		}
	}
	result := append([]string(nil), selected...)
	for _, item := range merged.Items {
		if item.ID == "" || seen[item.ID] {
			continue
		}
		result = append(result, item.ID)
		seen[item.ID] = true
	}
	return result
}

// mergeUserOverridesJSON takes a findings JSON payload and applies
// per-finding user instructions and user-authored findings. When no
// overrides are present the input is returned unchanged.
func mergeUserOverridesJSON(raw string, instructions map[string]string, added []types.Finding) string {
	if len(instructions) == 0 && len(added) == 0 {
		return raw
	}
	base, err := types.ParseFindingsJSON(raw)
	if err != nil {
		base = types.Findings{}
	}
	merged := types.MergeUserOverrides(base, instructions, added)
	encoded, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return raw
	}
	return encoded
}

func filterFindingsJSON(raw string, ids []string) string {
	if raw == "" {
		return raw
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	filtered := types.FilterFindings(findings, ids)
	if len(ids) == 0 {
		filtered = types.FindingsMetadata(findings)
		filtered.Summary = "0 selected findings"
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return raw
	}
	return filteredRaw
}

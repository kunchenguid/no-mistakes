package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// findingIDsJSON extracts the finding IDs from a findings JSON payload and
// returns them as a JSON array string. Empty result means there were no
// findings or parsing failed.
func findingIDsJSON(raw string) string {
	return marshalFindingIDs(findingIDList(raw))
}

// findingIDList extracts the finding IDs from a findings JSON payload as a
// plain slice. Empty/unparsable input returns nil.
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
		if item.ID == "" {
			continue
		}
		ids = append(ids, item.ID)
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

// matchingFindingIndex answers the same question as hasFindingMatch and, when
// the answer is yes, names which candidate matched so the caller can reconcile
// the two copies instead of silently keeping one.
func matchingFindingIndex(item types.Finding, byKey, byFingerprint map[types.Finding]int, itemCounts, candidateCounts map[types.Finding]int) (int, bool) {
	if index, ok := byKey[findingKey(item)]; ok {
		return index, true
	}
	fingerprint := findingFingerprint(item)
	if itemCounts[fingerprint] != 1 || candidateCounts[fingerprint] != 1 {
		return 0, false
	}
	index, ok := byFingerprint[fingerprint]
	return index, ok
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

// excludeFindingsJSON drops the named findings from a payload and returns
// what remains. An EMPTY id list resolves nothing, so it returns the payload
// unchanged rather than empty: this is the carry-forward set, and a fix
// response that selects no findings (a user-added finding on its own, or a
// bare `axi respond --action fix`) must leave every outstanding finding still
// outstanding. Only a payload that is itself empty, unparsable, or fully
// excluded yields "".
func excludeFindingsJSON(raw string, ids []string) string {
	if raw == "" {
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

// mergeUnique unions two lists of test-evidence metadata, fresh side first,
// dropping exact duplicates. The evidence a carried round produced is not
// re-stated by a later round that only reports its own new work, and the PR
// body reads the step's merged findings alone (testingEvidenceFindingsJSON
// never falls back to the round records once the merged payload carries any
// testing metadata), so anything dropped here is gone from the PR body.
func mergeUnique[T comparable](existing, additional []T) []T {
	if len(additional) == 0 {
		return existing
	}
	if len(existing) == 0 {
		return additional
	}
	seen := make(map[T]bool, len(existing)+len(additional))
	merged := make([]T, 0, len(existing)+len(additional))
	for _, entry := range existing {
		if seen[entry] {
			continue
		}
		seen[entry] = true
		merged = append(merged, entry)
	}
	for _, entry := range additional {
		if seen[entry] {
			continue
		}
		seen[entry] = true
		merged = append(merged, entry)
	}
	return merged
}

// stricterFindingAction returns whichever of the two actions blocks harder,
// on the no-op < auto-fix < ask-user scale. An empty action is ask-user (see
// types.actionOrDefault), so it can never be relaxed by omission.
func stricterFindingAction(carried, fresh string) string {
	if findingActionStrictness(fresh) > findingActionStrictness(carried) {
		return fresh
	}
	return carried
}

func findingActionStrictness(action string) int {
	switch action {
	case types.ActionNoOp:
		return 1
	case types.ActionAutoFix:
		return 2
	default:
		return 3
	}
}

// raisedRiskRationale explains an elevated level without republishing the
// carried assessment's prose, which was written about the earlier, larger
// finding set and can claim a count that no longer holds.
func raisedRiskRationale(freshRationale, level string, carriedCount int) string {
	raise := fmt.Sprintf("risk raised to %s: %d %s carried from an earlier round %s unresolved", level, carriedCount,
		pluralize(carriedCount, "finding", "findings"), pluralize(carriedCount, "remains", "remain"))
	fresh := strings.TrimSpace(freshRationale)
	if fresh == "" {
		return raise
	}
	return strings.TrimRight(fresh, ".;") + "; " + raise
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
	merged := types.Findings{Summary: existing.Summary, Tested: mergeUnique(existing.Tested, additional.Tested), TestingSummary: existing.TestingSummary, Artifacts: mergeUnique(existing.Artifacts, additional.Artifacts), RiskLevel: existing.RiskLevel, RiskRationale: existing.RiskRationale, RiskScope: existing.RiskScope}
	byKey := make(map[types.Finding]int, len(existing.Items))
	byFingerprint := make(map[types.Finding]int, len(existing.Items))
	for _, item := range existing.Items {
		index := len(merged.Items)
		merged.Items = append(merged.Items, item)
		key := findingKey(item)
		if _, ok := byKey[key]; !ok {
			byKey[key] = index
		}
		fingerprint := findingFingerprint(item)
		if _, ok := byFingerprint[fingerprint]; !ok {
			byFingerprint[fingerprint] = index
		}
		seen[key] = true
	}
	carried := 0
	for _, item := range additional.Items {
		if index, ok := matchingFindingIndex(item, byKey, byFingerprint, additionalCounts, existingCounts); ok {
			// This round restated a finding that is already outstanding. The
			// restatement is the round's own untrusted output, so it cannot
			// relax what the operator was already shown: the carried ID
			// survives, and the action can only move toward the stricter of
			// the two. A downgrade needs an explicit respond action or a real
			// auto-fix attempt; a genuine escalation still takes effect.
			merged.Items[index].Action = stricterFindingAction(item.Action, merged.Items[index].Action)
			if item.ID != "" {
				merged.Items[index].ID = item.ID
			}
			carried++
			continue
		}
		key := findingKey(item)
		if seen[key] {
			continue
		}
		merged.Items = append(merged.Items, item)
		seen[key] = true
		carried++
	}
	if len(merged.Items) == 0 {
		return ""
	}
	if carried > 0 {
		// The item list is now the union, so the fresh round's own summary and
		// risk level describe a strictly smaller set than what the gate shows.
		// Restate the count, and never present a risk level below the one the
		// still-unresolved carried findings were assessed at.
		merged.Summary = types.SummarizeOutstandingFindings(len(merged.Items))
		if raised := types.RiskLevelAtLeast(existing.RiskLevel, additional.RiskLevel); raised != existing.RiskLevel {
			merged.RiskLevel = raised
			merged.RiskRationale = raisedRiskRationale(existing.RiskRationale, raised, carried)
		}
	}
	mergedRaw, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return existingRaw
	}
	return mergedRaw
}

// dedupeFindingIDsJSON reassigns a fresh unique ID to any finding whose ID
// collides with an earlier finding's ID in the same payload. This matters
// only after mergeFindingsJSON: a round's own fresh output is normalized
// independently of what is being carried forward (positional IDs like
// "review-1" based only on that round's own item count), so a round that
// legitimately reports a genuinely new, unrelated finding can end up
// assigned the same ID as an different finding carried forward from an
// earlier round. Content-identical items are already deduplicated by
// mergeFindingsJSON's fingerprint match before this runs; this only
// separates two DIFFERENT findings that happen to share one ID, which would
// otherwise make ID-based selection (filter/exclude) silently apply to both.
//
// carriedRaw names the findings the operator has already been shown under
// their current IDs. Those keep their identity and the newly reported item is
// the one renamed, because an `axi respond --findings <id>` composed from an
// earlier gate read must keep selecting the finding it named.
func dedupeFindingIDsJSON(raw, prefix, carriedRaw string) string {
	if raw == "" {
		return raw
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	carried := carriedFindingIdentities(carriedRaw)
	seen := make(map[string]bool, len(findings.Items))
	settled := make([]bool, len(findings.Items))
	for i := range findings.Items {
		id := findings.Items[i].ID
		if id == "" || seen[id] {
			continue
		}
		if fingerprint, ok := carried[id]; !ok || fingerprint != findingFingerprint(findings.Items[i]) {
			continue
		}
		seen[id] = true
		settled[i] = true
	}
	counter := 0
	changed := false
	for i := range findings.Items {
		if settled[i] {
			continue
		}
		id := findings.Items[i].ID
		if id != "" && !seen[id] {
			seen[id] = true
			continue
		}
		for {
			counter++
			candidate := prefix + "-dup-" + strconv.Itoa(counter)
			if !seen[candidate] {
				findings.Items[i].ID = candidate
				seen[candidate] = true
				changed = true
				break
			}
		}
	}
	if !changed {
		return raw
	}
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw
	}
	return encoded
}

// carriedFindingIdentities maps each carried finding's ID to its content
// fingerprint, so a payload item can be recognized as that exact carried
// finding rather than merely as something that happens to share its ID. The
// fingerprint ignores Line for the same reason mergeFindingsJSON's match
// does: a restatement of the same defect normally reports a shifted line, and
// the merged item keeps the fresh line while inheriting the carried ID.
func carriedFindingIdentities(raw string) map[string]types.Finding {
	if raw == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	identities := make(map[string]types.Finding, len(findings.Items))
	for _, item := range findings.Items {
		if item.ID == "" {
			continue
		}
		if _, ok := identities[item.ID]; ok {
			continue
		}
		identities[item.ID] = findingFingerprint(item)
	}
	return identities
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
	filtered := types.Findings{Summary: existing.Summary, Tested: existing.Tested, TestingSummary: existing.TestingSummary, RiskLevel: existing.RiskLevel, RiskRationale: existing.RiskRationale, RiskScope: existing.RiskScope}
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
	filtered := types.Findings{Summary: existing.Summary, Tested: existing.Tested, TestingSummary: existing.TestingSummary, RiskLevel: existing.RiskLevel, RiskRationale: existing.RiskRationale, RiskScope: existing.RiskScope}
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
		filtered = types.Findings{
			Summary:        "0 selected findings",
			Tested:         findings.Tested,
			TestingSummary: findings.TestingSummary,
			RiskLevel:      findings.RiskLevel,
			RiskRationale:  findings.RiskRationale,
			RiskScope:      findings.RiskScope,
		}
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return raw
	}
	return filteredRaw
}

// unauthorizedApprovalClaim matches language asserting that a human user
// accepted, approved, authorized, or signed off on something. It is
// deliberately broad rather than trying to map phrasing to specific finding
// IDs: an agent's own generated rationale has no honest way to originate a
// claim of user acceptance mid-round - the only ground truth for that is an
// actual axi respond action, which the executor tracks separately - so any
// such claim surviving alongside findings this run cannot corroborate as
// resolved is fabricated by construction and must not reach storage or
// display untouched.
// The actor must be the literal "user". Widening this to other human-role
// nouns or to an agentless passive matched ordinary domain prose instead -
// "is authorized to execute commands", "force pushes are approved only when
// the lease anchor matches", "the developer confirmed the crash reproduces" -
// and a match discards the WHOLE rationale, which is then both what the
// operator reads at the parked gate and what the PR body publishes. The
// demonstrated fabrication names the user, so the pattern stays there.
var unauthorizedApprovalClaim = regexp.MustCompile(`(?i)(accepted|approved|authorized|authorised|confirmed|agreed|signed[- ]off|sign[- ]off|greenlit|green-lit|ok'd|okayed)\s+(by|from|per)\s+(the\s+)?user|user\s+(has\s+|already\s+)?(accepted|approved|authorized|authorised|confirmed|agreed|signed[- ]off|consented)`)

const fabricatedApprovalNotice = "risk rationale withheld: it asserted a user acceptance of findings that remain unresolved, with no corresponding axi respond action on record"

// sanitizeFabricatedApprovalJSON strips a risk_rationale that claims a human
// user accepted or approved findings when the findings payload still
// contains unresolved ask-user items. The executor is the only place that
// knows whether a matching respond action ever happened; a round's own
// generated rationale cannot honestly originate that claim on its own, so a
// matching claim found here is treated as fabricated and replaced rather
// than trusted. Reports whether it changed anything, for logging.
func sanitizeFabricatedApprovalJSON(raw string) (string, bool) {
	if raw == "" {
		return raw, false
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw, false
	}
	if !types.HasAskUserFindings(findings) {
		return raw, false
	}
	if findings.RiskRationale == "" || !unauthorizedApprovalClaim.MatchString(findings.RiskRationale) {
		return raw, false
	}
	findings.RiskRationale = fabricatedApprovalNotice
	sanitized, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw, false
	}
	return sanitized, true
}

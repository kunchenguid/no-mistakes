package pipeline

import (
	"encoding/json"

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
	merged := types.Findings{Summary: existing.Summary, Tested: existing.Tested, TestingSummary: existing.TestingSummary, RiskLevel: existing.RiskLevel, RiskRationale: existing.RiskRationale}
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
	filtered := types.Findings{Summary: existing.Summary, Tested: existing.Tested, TestingSummary: existing.TestingSummary, RiskLevel: existing.RiskLevel, RiskRationale: existing.RiskRationale}
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
	filtered := types.Findings{Summary: existing.Summary, Tested: existing.Tested, TestingSummary: existing.TestingSummary, RiskLevel: existing.RiskLevel, RiskRationale: existing.RiskRationale}
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
		}
	}
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return raw
	}
	return filteredRaw
}

package pipeline

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// filterSettledReviewFindings keeps a recorded human disposition effective when
// a later reviewer re-mints an ID or moves a finding's line. A fingerprint is
// accepted only when it identifies one finding in each report; ambiguous
// matches fail closed. Approve/skip/abort do not settle individual findings.
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
	ignored := make(map[types.Finding]bool)
	external := make(map[types.Finding]bool)
	fixed := make(map[types.Finding]bool)
	for i, round := range rounds {
		if round == nil || round.FindingsJSON == nil || round.SelectionSource == nil {
			continue
		}
		source := *round.SelectionSource
		// Before user_ignored was recorded separately, an ignore-only fix
		// used user_declined. A following fix round proves it was a fix
		// response rather than an approve/skip/abort verdict.
		legacyIgnore := source == db.RoundSelectionSourceUserDeclined && i+1 < len(rounds) &&
			(rounds[i+1].Trigger == "auto_fix" || rounds[i+1].Trigger == "user_fix")
		if source != db.RoundSelectionSourceUser && source != db.RoundSelectionSourceUserIgnored && source != db.RoundSelectionSourceExternal && !legacyIgnore {
			continue
		}
		shown, err := types.ParseFindingsJSON(*round.FindingsJSON)
		if err != nil {
			return "", fmt.Errorf("read review disposition: %w", err)
		}
		if round.SelectedFindingIDs == nil {
			return "", fmt.Errorf("review disposition is missing its selection")
		}
		var selected []string
		if err := json.Unmarshal([]byte(*round.SelectedFindingIDs), &selected); err != nil {
			return "", fmt.Errorf("read review selection: %w", err)
		}
		shownCounts := countFindingFingerprints(shown.Items)
		ids := make(map[string]bool, len(selected))
		for _, id := range selected {
			ids[id] = true
		}
		if source == db.RoundSelectionSourceExternal {
			if round.UserFindingsJSON == nil || len(selected) != 0 {
				return "", fmt.Errorf("external review disposition is incomplete")
			}
			resolved, err := types.ParseFindingsJSON(*round.UserFindingsJSON)
			if err != nil {
				return "", fmt.Errorf("read external review disposition: %w", err)
			}
			for _, item := range resolved.Items {
				if item.UserInstructions == "" {
					return "", fmt.Errorf("external review disposition is missing its reason")
				}
				for _, original := range shown.Items {
					if original.ID == item.ID && SameFinding(original, item) && shownCounts[findingFingerprint(original)] == 1 {
						external[findingFingerprint(original)] = true
					}
				}
			}
			continue
		}
		for _, item := range shown.Items {
			key := findingFingerprint(item)
			if shownCounts[key] != 1 {
				continue
			}
			if ids[item.ID] {
				fixed[key] = true
			} else {
				ignored[key] = true
			}
		}
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
		if counts[key] == 1 && !fixed[key] && (ignored[key] || external[key]) {
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

// completeReviewCoverage checks the full trusted scope, not merely whether the
// review recorded some paths. A settled finding cannot waive missing coverage.
func completeReviewCoverage(findings types.Findings) bool {
	scope := findings.ReviewCoverageScope
	if scope == nil || len(scope.Paths) == 0 || len(findings.ReviewedPaths) == 0 {
		return false
	}
	paths := make(map[string]bool, len(scope.Paths))
	for _, name := range scope.Paths {
		path := normalizeCoveredPath(name)
		if path == "" || paths[path] {
			return false
		}
		paths[path] = true
	}
	seen := make(map[string]bool, len(findings.ReviewedPaths))
	for _, name := range findings.ReviewedPaths {
		path := normalizeCoveredPath(name)
		if path == "" || !paths[path] {
			return false
		}
		seen[path] = true
	}
	return len(seen) == len(paths)
}

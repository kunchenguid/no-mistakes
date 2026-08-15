package eval

import (
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	matchExactID         = "exact-id"
	matchExactText       = "exact-text"
	matchLocation        = "location"
	matchContainment     = "containment"
	locationLineBand     = 3
	locationJaccardMin   = 0.5
	containmentMinTokens = 8
)

// Score is one candidate's finding-level confusion matrix against gold.
// Pending is unmatched candidate findings: queued, never punished as FP.
type Score struct {
	TruePositive      int
	TruePositiveExact int
	TruePositiveFuzzy int
	FalseNegative     int
	FalsePositive     int
	FalsePositiveGold int
	Pending           int
}

// ScoreCandidate matches a candidate finding list against recorded gold.
//
//   - TP: the candidate raises the same underlying issue as a true-issue gold
//     (human-accepted Fix, auto-fix that landed in a merged PR, a
//     human-added miss, or a confirmed post-PR miss)
//   - FN: the candidate misses a true-issue gold
//   - FP: only an explicit false-positive gold that the candidate still raised
//   - Pending: unmatched candidate findings, never inferred as invalid
//
// Matching is a documented cascade of strengths: exact-id, exact-text,
// nearby-line Jaccard, then gated containment. Assignment is maximum matching
// per strength tier so an earlier gold cannot consume a candidate that a later
// gold matches more strongly. Headline recall uses the full cascade; exact vs
// fuzzy counts are reported separately so a threshold change is visible.
func ScoreCandidate(labels Labels, findingsJSON string) Score {
	candidate := parseFindingItems(findingsJSON)
	assigned := assignMatches(labels.Findings, candidate)
	used := make([]bool, len(candidate))
	var score Score
	for i, gold := range labels.Findings {
		if gold.Kind == GoldFalsePositive {
			score.FalsePositiveGold++
		}
		match := assigned[i]
		switch {
		case isTrueIssueGold(gold.Kind) && match.cand >= 0:
			score.TruePositive++
			if match.strength == matchExactID || match.strength == matchExactText {
				score.TruePositiveExact++
			} else {
				score.TruePositiveFuzzy++
			}
			used[match.cand] = true
		case isTrueIssueGold(gold.Kind):
			score.FalseNegative++
		case gold.Kind == GoldFalsePositive && match.cand >= 0:
			score.FalsePositive++
			used[match.cand] = true
		}
	}
	for i := range candidate {
		if !used[i] {
			score.Pending++
		}
	}
	return score
}

func parseFindingItems(raw string) []types.Finding {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	return findings.Items
}

type assignedMatch struct {
	cand     int
	strength string
}

func assignMatches(golds []FindingGold, candidate []types.Finding) []assignedMatch {
	out := make([]assignedMatch, len(golds))
	for i := range out {
		out[i].cand = -1
	}
	matchedGold := make([]bool, len(golds))
	usedCand := make([]bool, len(candidate))
	for _, strength := range []string{matchExactID, matchExactText, matchLocation, matchContainment} {
		adj := make([][]int, len(golds))
		for gi, gold := range golds {
			if matchedGold[gi] {
				continue
			}
			for ci, finding := range candidate {
				if usedCand[ci] {
					continue
				}
				if matchAt(gold, finding, strength) {
					adj[gi] = append(adj[gi], ci)
				}
			}
		}
		goldToCand := maxBipartiteMatching(adj, len(candidate))
		for gi, ci := range goldToCand {
			if ci < 0 {
				continue
			}
			out[gi] = assignedMatch{cand: ci, strength: strength}
			matchedGold[gi] = true
			usedCand[ci] = true
		}
	}
	return out
}

func maxBipartiteMatching(adj [][]int, candCount int) []int {
	candToGold := make([]int, candCount)
	for i := range candToGold {
		candToGold[i] = -1
	}
	var dfs func(gi int, seen []bool) bool
	dfs = func(gi int, seen []bool) bool {
		for _, ci := range adj[gi] {
			if seen[ci] {
				continue
			}
			seen[ci] = true
			if candToGold[ci] < 0 || dfs(candToGold[ci], seen) {
				candToGold[ci] = gi
				return true
			}
		}
		return false
	}
	for gi := range adj {
		if len(adj[gi]) == 0 {
			continue
		}
		seen := make([]bool, candCount)
		_ = dfs(gi, seen)
	}
	goldToCand := make([]int, len(adj))
	for i := range goldToCand {
		goldToCand[i] = -1
	}
	for ci, gi := range candToGold {
		if gi >= 0 {
			goldToCand[gi] = ci
		}
	}
	return goldToCand
}

func matchAt(gold FindingGold, finding types.Finding, strength string) bool {
	switch strength {
	case matchExactID:
		return gold.ID != "" && gold.ID == finding.ID
	case matchExactText:
		return exactTextMatch(gold, finding)
	case matchLocation:
		return locationMatch(gold, finding)
	case matchContainment:
		return containmentMatch(gold, finding)
	default:
		return false
	}
}

func sameUnderlyingIssue(gold FindingGold, finding types.Finding) bool {
	return matchAt(gold, finding, matchExactID) || matchAt(gold, finding, matchExactText)
}

func exactTextMatch(gold FindingGold, finding types.Finding) bool {
	goldFile, goldDesc := normalizeIssue(gold.File, gold.Description)
	candFile, candDesc := normalizeIssue(finding.File, finding.Description)
	if goldFile == "" || candFile == "" || goldDesc == "" || candDesc == "" {
		return false
	}
	return goldFile == candFile && goldDesc == candDesc
}

func locationMatch(gold FindingGold, finding types.Finding) bool {
	goldFile, goldDesc := normalizeIssue(gold.File, gold.Description)
	candFile, candDesc := normalizeIssue(finding.File, finding.Description)
	if goldFile == "" || candFile == "" || goldDesc == "" || candDesc == "" {
		return false
	}
	if goldFile != candFile || gold.Line <= 0 || finding.Line <= 0 {
		return false
	}
	if absInt(gold.Line-finding.Line) > locationLineBand {
		return false
	}
	return tokenJaccard(goldDesc, candDesc) >= locationJaccardMin
}

func containmentMatch(gold FindingGold, finding types.Finding) bool {
	goldFile, goldDesc := normalizeIssue(gold.File, gold.Description)
	candFile, candDesc := normalizeIssue(finding.File, finding.Description)
	if goldFile == "" || candFile == "" || goldDesc == "" || candDesc == "" || goldFile != candFile {
		return false
	}
	if goldDesc == candDesc {
		return false
	}
	shorter, longer := goldDesc, candDesc
	if len(candDesc) < len(goldDesc) {
		shorter, longer = candDesc, goldDesc
	}
	if !strings.Contains(longer, shorter) {
		return false
	}
	return len(strings.Fields(shorter)) >= containmentMinTokens
}

func tokenJaccard(a, b string) float64 {
	left := uniqueTokens(a)
	right := uniqueTokens(b)
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	inter := 0
	for tok := range left {
		if right[tok] {
			inter++
		}
	}
	union := len(left) + len(right) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func uniqueTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.Fields(s) {
		out[tok] = true
	}
	return out
}

func normalizeIssue(file, description string) (string, string) {
	file = filepath.ToSlash(strings.TrimSpace(file))
	description = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(description))), " ")
	return file, description
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

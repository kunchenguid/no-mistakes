package eval

import (
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Score is one candidate's finding-level confusion matrix against gold.
// Pending is unmatched candidate findings: queued, never punished as FP.
type Score struct {
	TruePositive  int
	FalseNegative int
	FalsePositive int
	Pending       int
}

// ScoreCandidate matches a candidate finding list against recorded gold.
//
//   - TP: the candidate raises the same underlying issue as a true-issue gold
//     (human-accepted Fix, or a human-added miss the candidate also found)
//   - FN: the candidate misses a true-issue gold
//   - FP: only an explicit false-positive gold that the candidate still raised
//   - Pending: unmatched candidate findings, never inferred as invalid
func ScoreCandidate(labels Labels, findingsJSON string) Score {
	candidate := parseFindingItems(findingsJSON)
	used := make([]bool, len(candidate))
	var score Score
	for _, gold := range labels.Findings {
		match := firstUnusedMatch(gold, candidate, used)
		switch {
		case isTrueIssueGold(gold.Kind) && match >= 0:
			score.TruePositive++
			used[match] = true
		case isTrueIssueGold(gold.Kind):
			score.FalseNegative++
		case gold.Kind == GoldFalsePositive && match >= 0:
			score.FalsePositive++
			used[match] = true
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

func firstUnusedMatch(gold FindingGold, candidate []types.Finding, used []bool) int {
	for i, finding := range candidate {
		if used[i] {
			continue
		}
		if sameUnderlyingIssue(gold, finding) {
			return i
		}
	}
	return -1
}

func sameUnderlyingIssue(gold FindingGold, finding types.Finding) bool {
	if gold.ID != "" && gold.ID == finding.ID && !isRoundLocalFindingID(gold.ID) {
		return true
	}
	goldFile, goldDesc := normalizeIssue(gold.File, gold.Description)
	candFile, candDesc := normalizeIssue(finding.File, finding.Description)
	if goldDesc == "" || candDesc == "" {
		return false
	}
	return goldFile == candFile && goldDesc == candDesc
}

func isRoundLocalFindingID(id string) bool {
	for _, prefix := range []string{"review-", "user-"} {
		suffix, ok := strings.CutPrefix(id, prefix)
		if !ok || suffix == "" {
			continue
		}
		allDigits := true
		for _, char := range suffix {
			if char < '0' || char > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return true
		}
	}
	return false
}

func normalizeIssue(file, description string) (string, string) {
	file = filepath.ToSlash(strings.TrimSpace(file))
	description = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(description))), " ")
	return file, description
}

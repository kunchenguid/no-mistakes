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
// nearby-line Jaccard, then gated containment. Assignment is one globally
// optimal assignment over the whole graph (see assignMatches), so neither
// candidate ordering nor a tier boundary can consume a candidate another gold
// needed. Headline recall uses the full cascade; exact vs fuzzy counts are
// reported separately so a threshold change is visible.
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

// assignMatches pairs gold findings with candidate findings as one globally
// optimal assignment rather than a per-strength-tier pass.
//
// Tiered assignment - maximum matching on exact edges, then on the residual for
// each weaker strength - is not optimal for the whole graph, because WHICH
// maximum exact matching it happens to pick decides what the weaker tiers can
// still reach. With gold A matching candidate 1 exactly and candidate 2 by
// location, and gold B matching only candidate 1 exactly, the exact tier can
// hand candidate 1 to A and strand B forever, scoring one match where two exist.
// That understates recall for reasons that have nothing to do with the review
// under test, so the assignment is solved once over every edge at once.
//
// Optimality is lexicographic by strength: an exact pair is worth strictly more
// than every possible combination of weaker pairs (weightFor scales each tier by
// a base larger than any achievable count), so the optimum never trades one
// exact match for two fuzzy ones, and among the assignments with the most exact
// pairs it takes the one with the most location pairs, then the most containment
// pairs. hungarianMinCost solves that max-weight assignment exactly.
func assignMatches(golds []FindingGold, candidate []types.Finding) []assignedMatch {
	out := make([]assignedMatch, len(golds))
	for i := range out {
		out[i].cand = -1
	}
	if len(golds) == 0 || len(candidate) == 0 {
		return out
	}
	base := int64(len(golds)+len(candidate)) + 1
	weight := make([][]int64, len(golds))
	strength := make([][]string, len(golds))
	for gi, gold := range golds {
		weight[gi] = make([]int64, len(candidate))
		strength[gi] = make([]string, len(candidate))
		for ci, finding := range candidate {
			for _, s := range []string{matchExactID, matchExactText, matchLocation, matchContainment} {
				if matchAt(gold, finding, s) {
					weight[gi][ci] = weightFor(s, base)
					strength[gi][ci] = s
					break
				}
			}
		}
	}
	for gi, ci := range maxWeightAssignment(weight) {
		// A zero-weight pair is the absence of an edge, not a match: the
		// assignment is over a complete matrix so every row gets a column.
		if ci < 0 || weight[gi][ci] == 0 {
			continue
		}
		out[gi] = assignedMatch{cand: ci, strength: strength[gi][ci]}
	}
	return out
}

// weightFor ranks the strengths so that no number of weaker pairs can outweigh
// a single stronger one. base exceeds the largest possible pair count, so the
// total weight of an assignment reads as a positional number whose digits are
// the per-strength counts.
func weightFor(strength string, base int64) int64 {
	switch strength {
	case matchExactID, matchExactText:
		return base * base
	case matchLocation:
		return base
	case matchContainment:
		return 1
	default:
		return 0
	}
}

// maxWeightAssignment returns, for each gold row, the candidate column assigned
// to it (-1 when there are no columns), maximizing total weight. It solves the
// rectangular assignment problem exactly, transposing when there are more rows
// than columns because the solver requires rows <= columns.
func maxWeightAssignment(weight [][]int64) []int {
	rows, cols := len(weight), len(weight[0])
	if rows <= cols {
		return hungarianMinCost(negate(weight))
	}
	transposed := make([][]int64, cols)
	for j := range transposed {
		transposed[j] = make([]int64, rows)
		for i := range weight {
			transposed[j][i] = -weight[i][j]
		}
	}
	colToRow := hungarianMinCost(transposed)
	rowToCol := make([]int, rows)
	for i := range rowToCol {
		rowToCol[i] = -1
	}
	for j, i := range colToRow {
		if i >= 0 {
			rowToCol[i] = j
		}
	}
	return rowToCol
}

func negate(weight [][]int64) [][]int64 {
	out := make([][]int64, len(weight))
	for i, row := range weight {
		out[i] = make([]int64, len(row))
		for j, w := range row {
			out[i][j] = -w
		}
	}
	return out
}

// hungarianMinCost is the O(n^2*m) Hungarian (Kuhn-Munkres) algorithm for the
// rectangular assignment problem: it returns the minimum-cost assignment of
// every row to a distinct column, which is the exact optimum rather than a
// greedy approximation. It requires len(cost) <= len(cost[0]).
func hungarianMinCost(cost [][]int64) []int {
	n := len(cost)
	if n == 0 {
		return nil
	}
	m := len(cost[0])
	const inf = int64(1) << 62
	// Potentials u/v and the column-to-row matching p are 1-indexed; index 0 is
	// the algorithm's virtual starting column.
	u := make([]int64, n+1)
	v := make([]int64, m+1)
	p := make([]int, m+1)
	way := make([]int, m+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]int64, m+1)
		used := make([]bool, m+1)
		for j := range minv {
			minv[j] = inf
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := inf
			j1 := 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := cost[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}
	rowToCol := make([]int, n)
	for i := range rowToCol {
		rowToCol[i] = -1
	}
	for j := 1; j <= m; j++ {
		if p[j] > 0 {
			rowToCol[p[j]-1] = j - 1
		}
	}
	return rowToCol
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

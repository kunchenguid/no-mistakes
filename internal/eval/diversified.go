package eval

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type diversifiedPin struct {
	CaseID   string
	Stratum  string
	Rank     int
	PinnedAt int64
}

func diversifiedStratum(c Case) string {
	language, size, severity := caseComposition(c)
	return strings.Join([]string{c.RepoFingerprint, language, size, severity, findingType(c)}, "\x00")
}

func findingType(c Case) string {
	hasTrueIssue := false
	hasFP := false
	bestSev := ""
	bestAction := ""
	sevRank := map[string]int{"none": 0, "info": 1, "warning": 2, "error": 3}
	byID := roundFindingsByID(c)
	for _, gold := range c.Labels.Findings {
		switch {
		case isTrueIssueGold(gold.Kind):
			hasTrueIssue = true
			sev := gold.Severity
			action := gold.Action
			if original, ok := byID[strings.TrimSpace(gold.ID)]; ok {
				if sev == "" {
					sev = original.Severity
				}
				if action == "" {
					action = original.Action
				}
			}
			if sev == "" {
				sev = "none"
			}
			if sevRank[sev] > sevRank[bestSev] || (sevRank[sev] == sevRank[bestSev] && (bestAction == "" || action < bestAction)) {
				bestSev = sev
				bestAction = action
			}
		case gold.Kind == GoldFalsePositive:
			hasFP = true
		}
	}
	if hasTrueIssue {
		if bestSev == "" {
			bestSev = "none"
		}
		return bestSev + "/" + bestAction
	}
	if hasFP {
		return "false-positive"
	}
	return "unlabeled"
}

func roundFindingsByID(c Case) map[string]struct{ Severity, Action string } {
	out := map[string]struct{ Severity, Action string }{}
	raw, err := osReadRoundFindings(c)
	if err != nil || raw == "" {
		return out
	}
	for _, finding := range parseFindingItems(raw) {
		id := strings.TrimSpace(finding.ID)
		if id == "" {
			continue
		}
		out[id] = struct{ Severity, Action string }{finding.Severity, finding.Action}
	}
	return out
}

func osReadRoundFindings(c Case) (string, error) {
	if strings.TrimSpace(c.Dir) == "" {
		return "", nil
	}
	var record sourceRound
	if err := readJSON(joinOriginalRound(c.Dir), &record); err != nil {
		return "", err
	}
	if record.FindingsJSON == nil {
		return "", nil
	}
	return *record.FindingsJSON, nil
}

func joinOriginalRound(dir string) string {
	return filepath.Join(dir, "original", "round.json")
}

func rankedStrata(gold []Case) (map[string][]Case, []string) {
	grouped := map[string][]Case{}
	for _, c := range gold {
		if !c.Labels.HasGold() {
			continue
		}
		key := diversifiedStratum(c)
		grouped[key] = append(grouped[key], c)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
		sort.Slice(grouped[key], func(i, j int) bool {
			a, b := grouped[key][i], grouped[key][j]
			if ac, bc := a.Labels.TrueIssueCount(), b.Labels.TrueIssueCount(); ac != bc {
				return ac > bc
			}
			if a.CapturedAt != b.CapturedAt {
				return a.CapturedAt < b.CapturedAt
			}
			return a.ID < b.ID
		})
	}
	sort.Strings(keys)
	return grouped, keys
}

func planDiversified(gold []Case, cap int, existing []diversifiedPin) []diversifiedPin {
	grouped, keys := rankedStrata(gold)
	byID := map[string]Case{}
	for _, c := range gold {
		byID[c.ID] = c
	}

	kept := make([]diversifiedPin, 0, len(existing))
	pinned := map[string]bool{}
	now := time.Now().Unix()
	for _, pin := range existing {
		c, ok := byID[pin.CaseID]
		if !ok || !c.Labels.HasGold() {
			continue
		}
		if pin.Stratum == "" {
			pin.Stratum = diversifiedStratum(c)
		}
		kept = append(kept, pin)
		pinned[pin.CaseID] = true
	}

	seatsFor := func(stratum string, n int) []diversifiedPin {
		cases := grouped[stratum]
		if n > len(cases) {
			n = len(cases)
		}
		out := make([]diversifiedPin, 0, n)
		rank := 1
		for _, c := range cases {
			if len(out) >= n {
				break
			}
			if pinned[c.ID] {
				continue
			}
			out = append(out, diversifiedPin{CaseID: c.ID, Stratum: stratum, Rank: rank, PinnedAt: now})
			rank++
		}
		return out
	}

	if cap == 0 {
		kept = onePinPerStratum(kept)
		pinned = pinSet(kept)
		for _, key := range keys {
			already := 0
			for _, pin := range kept {
				if pin.Stratum == key {
					already++
				}
			}
			if already > 0 {
				continue
			}
			kept = append(kept, seatsFor(key, 1)...)
			pinned = pinSet(kept)
		}
		return kept
	}

	collapsed := false
	if len(kept) > cap {
		kept = onePinPerStratum(kept)
		if len(kept) > cap {
			kept = append([]diversifiedPin(nil), kept[:cap]...)
		}
		pinned = pinSet(kept)
		collapsed = true
	}

	remaining := cap - len(kept)
	if remaining <= 0 {
		return kept
	}

	unpinnedKeys := make([]string, 0, len(keys))
	weights := make([]int, 0, len(keys))
	capacities := make([]int, 0, len(keys))
	occupied := map[string]int{}
	for _, pin := range kept {
		occupied[pin.Stratum]++
	}
	for _, key := range keys {
		if occupied[key] > 0 {
			continue
		}
		unpinnedKeys = append(unpinnedKeys, key)
		weights = append(weights, len(grouped[key]))
		capacity := len(grouped[key])
		if collapsed && capacity > 1 {
			// Cap shrink already collapsed duplicates; leftover seats may
			// fill new strata but must not recreate k>1 in one of them.
			capacity = 1
		}
		capacities = append(capacities, capacity)
	}
	if len(unpinnedKeys) == 0 {
		return kept
	}
	seats := hamiltonSeats(weights, remaining, capacities)
	for i, key := range unpinnedKeys {
		added := seatsFor(key, seats[i])
		kept = append(kept, added...)
		for _, pin := range added {
			pinned[pin.CaseID] = true
		}
	}
	return kept
}

func onePinPerStratum(pins []diversifiedPin) []diversifiedPin {
	seen := map[string]bool{}
	out := make([]diversifiedPin, 0, len(pins))
	for _, pin := range pins {
		if seen[pin.Stratum] {
			continue
		}
		seen[pin.Stratum] = true
		out = append(out, pin)
	}
	return out
}

func pinSet(pins []diversifiedPin) map[string]bool {
	out := map[string]bool{}
	for _, pin := range pins {
		out[pin.CaseID] = true
	}
	return out
}

func hamiltonSeats(weights []int, cap int, capacities []int) []int {
	n := len(weights)
	seats := make([]int, n)
	if cap <= 0 || n == 0 {
		return seats
	}
	total := 0
	for _, w := range weights {
		total += w
	}
	if total == 0 {
		return seats
	}
	type remainder struct {
		i    int
		frac float64
		key  int
	}
	allocated := 0
	remainders := make([]remainder, 0, n)
	for i, w := range weights {
		quota := float64(cap) * float64(w) / float64(total)
		floor := int(quota)
		if floor > capacities[i] {
			floor = capacities[i]
		}
		if floor < 0 {
			floor = 0
		}
		seats[i] = floor
		allocated += floor
		remainders = append(remainders, remainder{i: i, frac: quota - float64(int(quota)), key: i})
	}
	sort.SliceStable(remainders, func(a, b int) bool {
		if remainders[a].frac != remainders[b].frac {
			return remainders[a].frac > remainders[b].frac
		}
		return remainders[a].key < remainders[b].key
	})
	for allocated < cap {
		progressed := false
		for _, rem := range remainders {
			if allocated >= cap {
				break
			}
			if seats[rem.i] >= capacities[rem.i] {
				continue
			}
			seats[rem.i]++
			allocated++
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return seats
}

func casesByPinOrder(all []Case, pins []diversifiedPin) []Case {
	byID := map[string]Case{}
	for _, c := range all {
		byID[c.ID] = c
	}
	out := make([]Case, 0, len(pins))
	for _, pin := range pins {
		c, ok := byID[pin.CaseID]
		if !ok || !c.Labels.HasGold() {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CapturedAt != out[j].CapturedAt {
			return out[i].CapturedAt < out[j].CapturedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func labeledCases(all []Case) []Case {
	out := make([]Case, 0, len(all))
	for _, c := range all {
		if c.Labels.HasGold() {
			out = append(out, c)
		}
	}
	return out
}

package tui

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (m Model) awaitingActionState() (showSelectionActions bool, allowFix bool, selectedCount int, totalCount int) {
	step := awaitingStep(m.steps)
	if step == nil {
		return false, false, 0, 0
	}
	items := m.findingItems(step.StepName)
	if len(items) == 0 {
		return false, false, 0, 0
	}
	totalCount = len(items)
	selected, ok := m.findingSelections[step.StepName]
	if !ok {
		return true, true, totalCount, totalCount
	}
	selectedCount = len(selected)
	return true, selectedCount > 0, selectedCount, totalCount
}

// findingItems returns the complete list of findings rendered for the step,
// agent-produced items first followed by any user-added findings. Cursor
// positions and selection state are tracked against this combined list.
func (m *Model) findingItems(step types.StepName) []finding {
	raw := m.stepFindings[step]
	var items []finding
	if f, err := parseFindings(raw); err == nil && f != nil {
		items = append(items, f.Items...)
	}
	if added, ok := m.addedFindings[step]; ok {
		items = append(items, added...)
	}
	return items
}

// agentFindingItems returns only the agent-produced findings for the step,
// used when the caller must distinguish user-authored findings from the
// rest (e.g. when building RespondParams).
func (m *Model) agentFindingItems(step types.StepName) []finding {
	raw := m.stepFindings[step]
	f, err := parseFindings(raw)
	if err != nil || f == nil {
		return nil
	}
	return f.Items
}

// combinedFindingsJSON returns the findings JSON for the step merged with
// any user overrides (per-finding instructions) and user-added findings.
// If no overrides exist, the original JSON is returned unchanged.
func (m *Model) combinedFindingsJSON(step types.StepName) string {
	raw := m.stepFindings[step]
	instructions := m.findingInstructions[step]
	added := m.addedFindings[step]
	if len(instructions) == 0 && len(added) == 0 {
		return raw
	}
	base, err := parseFindings(raw)
	if err != nil || base == nil {
		base = &findings{}
	}
	merged := types.MergeUserOverrides(*base, instructions, added)
	encoded, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return raw
	}
	return encoded
}

func (m *Model) ensureFindingSelection(step types.StepName) {
	if m.findingSelections == nil {
		m.findingSelections = make(map[types.StepName]map[string]bool)
	}
	if m.findingCursor == nil {
		m.findingCursor = make(map[types.StepName]int)
	}
	if _, ok := m.findingSelections[step]; ok {
		return
	}
	m.resetFindingSelection(step)
}

func (m *Model) resetFindingSelection(step types.StepName) {
	if m.findingSelections == nil {
		m.findingSelections = make(map[types.StepName]map[string]bool)
	}
	if m.findingCursor == nil {
		m.findingCursor = make(map[types.StepName]int)
	}
	selected := make(map[string]bool)
	for _, item := range m.findingItems(step) {
		if item.ID != "" {
			selected[item.ID] = true
		}
	}
	m.findingSelections[step] = selected
	m.findingCursor[step] = 0
}

// selectedFindingIDs returns the IDs of selected agent-produced findings.
// User-authored findings are conveyed via addedFindings separately because
// the daemon only recognizes agent IDs in the FindingIDs list.
func (m *Model) selectedFindingIDs(step types.StepName) []string {
	selected := m.findingSelections[step]
	if len(selected) == 0 {
		return nil
	}
	var ids []string
	for _, item := range m.agentFindingItems(step) {
		if selected[item.ID] {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// selectedUserAddedFindings returns the user-added findings that are
// currently selected (and therefore should be sent to the fix agent).
func (m *Model) selectedUserAddedFindings(step types.StepName) []finding {
	added := m.addedFindings[step]
	if len(added) == 0 {
		return nil
	}
	selected := m.findingSelections[step]
	var result []finding
	for _, item := range added {
		if selected == nil || selected[item.ID] {
			result = append(result, item)
		}
	}
	return result
}

// diffOffsetForCurrentFinding returns the diff scroll offset that corresponds
// to the current finding's file:line. Returns 0 if no match.
func (m Model) diffOffsetForCurrentFinding(step types.StepName) int {
	items := m.findingItems(step)
	if len(items) == 0 {
		return 0
	}
	cursor := m.findingCursor[step]
	if cursor < 0 || cursor >= len(items) {
		return 0
	}
	item := items[cursor]
	if item.File == "" {
		return 0
	}
	raw := m.stepDiffs[step]
	if raw == "" {
		return 0
	}
	lines := parseDiffLines(raw)
	return findDiffOffset(lines, item.File, item.Line)
}

func (m *Model) moveFindingCursor(step types.StepName, delta int) {
	items := m.findingItems(step)
	if len(items) == 0 {
		return
	}
	cur := m.findingCursor[step] + delta
	if cur < 0 {
		cur = 0
	}
	if cur >= len(items) {
		cur = len(items) - 1
	}
	m.findingCursor[step] = cur
}

func (m *Model) toggleCurrentFinding(step types.StepName) {
	items := m.findingItems(step)
	if len(items) == 0 {
		return
	}
	m.ensureFindingSelection(step)
	cur := m.findingCursor[step]
	if cur < 0 || cur >= len(items) {
		return
	}
	id := items[cur].ID
	if id == "" {
		return
	}
	m.findingSelections[step][id] = !m.findingSelections[step][id]
	if !m.findingSelections[step][id] {
		delete(m.findingSelections[step], id)
	}
	if m.findingSelections[step] == nil {
		m.findingSelections[step] = make(map[string]bool)
	}
	if m.findingSelections[step][id] {
		return
	}
}

func (m *Model) selectAllFindings(step types.StepName) {
	m.resetFindingSelection(step)
}

func (m *Model) clearAllFindings(step types.StepName) {
	m.ensureFindingSelection(step)
	m.findingSelections[step] = make(map[string]bool)
}

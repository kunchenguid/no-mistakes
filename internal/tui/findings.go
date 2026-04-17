package tui

import "github.com/kunchenguid/no-mistakes/internal/types"

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

func (m *Model) findingItems(step types.StepName) []finding {
	raw := m.stepFindings[step]
	f, err := parseFindings(raw)
	if err != nil || f == nil {
		return nil
	}
	return f.Items
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

func (m *Model) selectedFindingIDs(step types.StepName) []string {
	selected := m.findingSelections[step]
	if len(selected) == 0 {
		return nil
	}
	var ids []string
	for _, item := range m.findingItems(step) {
		if selected[item.ID] {
			ids = append(ids, item.ID)
		}
	}
	return ids
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

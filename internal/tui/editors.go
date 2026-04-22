package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// editorKind identifies which modal editor is currently active.
type editorKind int

const (
	editorNone editorKind = iota
	editorInstruction
	editorAddFinding
)

// addFindingField identifies the currently focused input in the add-finding modal.
type addFindingField int

const (
	addFieldSeverity addFindingField = iota
	addFieldFile
	addFieldLine
	addFieldDescription
	addFieldInstruction
	addFieldCount
)

var addFindingSeverities = []string{"error", "warning", "info"}

// editorState holds in-progress state for the currently open modal editor.
// Only one editor is active at a time; m.editor is nil when none is open.
type editorState struct {
	kind     editorKind
	step     types.StepName
	errorMsg string

	// instruction editor fields
	findingID   string
	instruction textarea.Model

	// add-finding editor fields
	addSeverityIdx int
	addFile        textinput.Model
	addLine        textinput.Model
	addDesc        textarea.Model
	addInstr       textarea.Model
	addFocus       addFindingField
}

func newInstructionEditor(step types.StepName, findingID, existing string) *editorState {
	ta := textarea.New()
	ta.Placeholder = "extra guidance for the fix agent (optional)..."
	ta.SetValue(existing)
	ta.ShowLineNumbers = false
	ta.CharLimit = 2000
	ta.SetHeight(5)
	ta.SetWidth(72)
	ta.Focus()
	return &editorState{
		kind:        editorInstruction,
		step:        step,
		findingID:   findingID,
		instruction: ta,
	}
}

func newAddFindingEditor(step types.StepName) *editorState {
	file := textinput.New()
	file.Placeholder = "file (optional, e.g. internal/foo/bar.go)"
	file.CharLimit = 500
	file.Width = 70

	line := textinput.New()
	line.Placeholder = "line (optional)"
	line.CharLimit = 10
	line.Width = 12
	line.Validate = func(s string) error {
		if s == "" {
			return nil
		}
		if _, err := strconv.Atoi(s); err != nil {
			return fmt.Errorf("must be a number")
		}
		return nil
	}

	desc := textarea.New()
	desc.Placeholder = "what's the issue (required)..."
	desc.ShowLineNumbers = false
	desc.CharLimit = 4000
	desc.SetHeight(3)
	desc.SetWidth(72)

	instr := textarea.New()
	instr.Placeholder = "extra guidance for the fix agent (optional)..."
	instr.ShowLineNumbers = false
	instr.CharLimit = 2000
	instr.SetHeight(3)
	instr.SetWidth(72)

	e := &editorState{
		kind:     editorAddFinding,
		step:     step,
		addFile:  file,
		addLine:  line,
		addDesc:  desc,
		addInstr: instr,
		addFocus: addFieldSeverity,
	}
	return e
}

// isActive reports whether an editor modal is currently open.
func (m Model) editorActive() bool { return m.editor != nil && m.editor.kind != editorNone }

// updateEditor routes a key event to the active editor. It returns the
// possibly-updated model plus any tea.Cmd produced by the underlying
// bubble components. When the editor consumes the key (anything that is
// not an escape/save shortcut) the caller should not fall through to the
// app-level key handling.
func (m Model) updateEditor(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.editor == nil {
		return m, nil
	}
	switch m.editor.kind {
	case editorInstruction:
		return m.updateInstructionEditor(msg)
	case editorAddFinding:
		return m.updateAddFindingEditor(msg)
	}
	return m, nil
}

func (m Model) updateInstructionEditor(msg tea.KeyMsg) (Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		m.editor = nil
		return m, nil
	case "ctrl+s", "ctrl+enter":
		m.saveInstruction()
		return m, nil
	}
	var cmd tea.Cmd
	m.editor.instruction, cmd = m.editor.instruction.Update(msg)
	return m, cmd
}

func (m *Model) saveInstruction() {
	if m.editor == nil || m.editor.kind != editorInstruction {
		return
	}
	step := m.editor.step
	id := m.editor.findingID
	text := strings.TrimSpace(m.editor.instruction.Value())
	if m.findingInstructions == nil {
		m.findingInstructions = make(map[types.StepName]map[string]string)
	}
	if m.findingInstructions[step] == nil {
		m.findingInstructions[step] = make(map[string]string)
	}
	if text == "" {
		delete(m.findingInstructions[step], id)
		if len(m.findingInstructions[step]) == 0 {
			delete(m.findingInstructions, step)
		}
	} else {
		m.findingInstructions[step][id] = text
	}
	m.editor = nil
}

func (m Model) updateAddFindingEditor(msg tea.KeyMsg) (Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		m.editor = nil
		return m, nil
	case "ctrl+s":
		if err := m.saveAddFinding(); err != nil {
			m.editor.errorMsg = err.Error()
			return m, nil
		}
		return m, nil
	case "tab":
		m.editor.addFocus = (m.editor.addFocus + 1) % addFieldCount
		m.applyAddFindingFocus()
		return m, nil
	case "shift+tab":
		m.editor.addFocus = (m.editor.addFocus + addFieldCount - 1) % addFieldCount
		m.applyAddFindingFocus()
		return m, nil
	}

	if m.editor.addFocus == addFieldSeverity {
		switch key {
		case "left", "h":
			if m.editor.addSeverityIdx > 0 {
				m.editor.addSeverityIdx--
			}
			return m, nil
		case "right", "l":
			if m.editor.addSeverityIdx < len(addFindingSeverities)-1 {
				m.editor.addSeverityIdx++
			}
			return m, nil
		}
		return m, nil
	}

	var cmd tea.Cmd
	switch m.editor.addFocus {
	case addFieldFile:
		m.editor.addFile, cmd = m.editor.addFile.Update(msg)
	case addFieldLine:
		m.editor.addLine, cmd = m.editor.addLine.Update(msg)
	case addFieldDescription:
		m.editor.addDesc, cmd = m.editor.addDesc.Update(msg)
	case addFieldInstruction:
		m.editor.addInstr, cmd = m.editor.addInstr.Update(msg)
	}
	return m, cmd
}

func (m *Model) applyAddFindingFocus() {
	m.editor.addFile.Blur()
	m.editor.addLine.Blur()
	m.editor.addDesc.Blur()
	m.editor.addInstr.Blur()
	switch m.editor.addFocus {
	case addFieldFile:
		m.editor.addFile.Focus()
	case addFieldLine:
		m.editor.addLine.Focus()
	case addFieldDescription:
		m.editor.addDesc.Focus()
	case addFieldInstruction:
		m.editor.addInstr.Focus()
	}
}

func (m *Model) saveAddFinding() error {
	if m.editor == nil || m.editor.kind != editorAddFinding {
		return nil
	}
	desc := strings.TrimSpace(m.editor.addDesc.Value())
	if desc == "" {
		return fmt.Errorf("description is required")
	}
	line := 0
	if raw := strings.TrimSpace(m.editor.addLine.Value()); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("line must be a number")
		}
		line = n
	}
	f := types.Finding{
		Severity:         addFindingSeverities[m.editor.addSeverityIdx],
		File:             strings.TrimSpace(m.editor.addFile.Value()),
		Line:             line,
		Description:      desc,
		Action:           types.ActionAutoFix,
		Source:           types.FindingSourceUser,
		UserInstructions: strings.TrimSpace(m.editor.addInstr.Value()),
	}
	step := m.editor.step
	if m.addedFindings == nil {
		m.addedFindings = make(map[types.StepName][]types.Finding)
	}
	assignedID := m.nextUserFindingID(step)
	f.ID = assignedID
	m.addedFindings[step] = append(m.addedFindings[step], f)
	if m.findingSelections == nil {
		m.findingSelections = make(map[types.StepName]map[string]bool)
	}
	if m.findingSelections[step] == nil {
		m.findingSelections[step] = make(map[string]bool)
	}
	m.findingSelections[step][assignedID] = true
	m.editor = nil
	return nil
}

// nextUserFindingID returns a unique "user-N" ID that does not collide
// with any agent-produced finding or prior user-added finding for this step.
func (m Model) nextUserFindingID(step types.StepName) string {
	used := make(map[string]bool)
	for _, item := range m.findingItems(step) {
		if item.ID != "" {
			used[item.ID] = true
		}
	}
	for _, item := range m.addedFindings[step] {
		if item.ID != "" {
			used[item.ID] = true
		}
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("user-%d", i)
		if !used[candidate] {
			return candidate
		}
	}
}

// removeUserFinding removes a user-added finding by ID from the given step
// if present. Returns true when a removal occurred.
func (m *Model) removeUserFinding(step types.StepName, id string) bool {
	items := m.addedFindings[step]
	for i, item := range items {
		if item.ID == id {
			m.addedFindings[step] = append(items[:i], items[i+1:]...)
			if len(m.addedFindings[step]) == 0 {
				delete(m.addedFindings, step)
			}
			if sel := m.findingSelections[step]; sel != nil {
				delete(sel, id)
			}
			return true
		}
	}
	return false
}

// findingAtCursor returns the finding currently highlighted in the findings
// list for the given step, combining agent-produced and user-added items.
// Returns false when the cursor is out of range.
func (m Model) findingAtCursor(step types.StepName) (types.Finding, bool) {
	all := m.combinedFindingItems(step)
	if len(all) == 0 {
		return types.Finding{}, false
	}
	cur := m.findingCursor[step]
	if cur < 0 || cur >= len(all) {
		return types.Finding{}, false
	}
	return all[cur], true
}

// combinedFindingItems returns the full list of findings for a step as
// rendered: agent-produced items first, user-added items appended.
func (m Model) combinedFindingItems(step types.StepName) []types.Finding {
	items := m.findingItems(step)
	result := make([]types.Finding, 0, len(items)+len(m.addedFindings[step]))
	result = append(result, items...)
	result = append(result, m.addedFindings[step]...)
	return result
}

// renderInstructionEditor renders the instruction editor overlay.
func (m Model) renderInstructionEditor(width int) string {
	if m.editor == nil || m.editor.kind != editorInstruction {
		return ""
	}
	boxWidth := width
	if boxWidth < 40 {
		boxWidth = 40
	}
	contentWidth := boxWidth - 4
	if contentWidth < 20 {
		contentWidth = 20
	}
	m.editor.instruction.SetWidth(contentWidth)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiCyan))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	title := titleStyle.Render(fmt.Sprintf("Instruction for %s", m.editor.findingID))

	var body strings.Builder
	body.WriteString(m.editor.instruction.View())
	body.WriteString("\n")
	body.WriteString(dimStyle.Render("ctrl+s save  ·  esc cancel"))

	return renderBoxWithStyledTitle(title, body.String(), boxWidth, "")
}

// renderAddFindingEditor renders the add-finding editor overlay.
func (m Model) renderAddFindingEditor(width int) string {
	if m.editor == nil || m.editor.kind != editorAddFinding {
		return ""
	}
	boxWidth := width
	if boxWidth < 40 {
		boxWidth = 40
	}
	contentWidth := boxWidth - 4
	if contentWidth < 20 {
		contentWidth = 20
	}
	m.editor.addFile.Width = contentWidth - 2
	m.editor.addDesc.SetWidth(contentWidth)
	m.editor.addInstr.SetWidth(contentWidth)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiCyan))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	focusStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiCyan))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	label := func(field addFindingField, text string) string {
		if m.editor.addFocus == field {
			return focusStyle.Render("> " + text)
		}
		return labelStyle.Render("  " + text)
	}

	// Severity row
	sevRow := ""
	for i, name := range addFindingSeverities {
		marker := "  "
		style := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
		if i == m.editor.addSeverityIdx {
			marker = "● "
			style = severityStyle(name).Bold(true)
		} else {
			marker = "○ "
		}
		sevRow += style.Render(marker+name) + "  "
	}

	var body strings.Builder
	body.WriteString(label(addFieldSeverity, "Severity"))
	body.WriteString("\n  ")
	body.WriteString(sevRow)
	body.WriteString("\n\n")
	body.WriteString(label(addFieldFile, "File"))
	body.WriteString("\n")
	body.WriteString("  " + m.editor.addFile.View())
	body.WriteString("\n\n")
	body.WriteString(label(addFieldLine, "Line"))
	body.WriteString("\n")
	body.WriteString("  " + m.editor.addLine.View())
	body.WriteString("\n\n")
	body.WriteString(label(addFieldDescription, "Description"))
	body.WriteString("\n")
	body.WriteString(m.editor.addDesc.View())
	body.WriteString("\n\n")
	body.WriteString(label(addFieldInstruction, "Fix instruction (optional)"))
	body.WriteString("\n")
	body.WriteString(m.editor.addInstr.View())
	body.WriteString("\n\n")
	if m.editor.errorMsg != "" {
		errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
		body.WriteString(errStyle.Render("! " + m.editor.errorMsg))
		body.WriteString("\n")
	}
	body.WriteString(dimStyle.Render("tab next field  ·  ctrl+s save  ·  esc cancel"))

	title := titleStyle.Render("Add finding")
	return renderBoxWithStyledTitle(title, body.String(), boxWidth, "")
}

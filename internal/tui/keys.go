package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Route to modal editor first, if one is open. ctrl+c always quits.
	if m.editorActive() && key != "ctrl+c" {
		updated, cmd := m.updateEditor(msg)
		return updated, cmd
	}

	// Reset abort confirmation on any key except 'x'.
	if key != "x" {
		m.confirmAbort = false
	}

	// Auto-dismiss help on any key except ? (toggle) and esc (handled below).
	if m.showHelp && key != "?" && key != "esc" {
		m.showHelp = false
	}

	switch key {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Sequence(tea.SetWindowTitle(""), tea.Quit)

	case "?":
		m.showHelp = !m.showHelp
		return m, nil

	case "esc":
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		if m.showDiff {
			m.showDiff = false
			m.diffOffset = 0
			return m, nil
		}

	case "d":
		if step := awaitingStep(m.steps); step != nil {
			if raw, ok := m.stepDiffs[step.StepName]; ok && raw != "" {
				m.showDiff = !m.showDiff
				if m.showDiff {
					m.diffOffset = m.diffOffsetForCurrentFinding(step.StepName)
				} else {
					m.diffOffset = 0
				}
			}
		}
		return m, nil

	case "n":
		if m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.moveFindingCursor(step.StepName, 1)
				m.diffOffset = m.diffOffsetForCurrentFinding(step.StepName)
			}
		}
		return m, nil

	case "p":
		if m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.moveFindingCursor(step.StepName, -1)
				m.diffOffset = m.diffOffsetForCurrentFinding(step.StepName)
			}
		}
		return m, nil

	case "j", "down":
		if m.showDiff {
			m.diffOffset++
		} else if step := awaitingStep(m.steps); step != nil {
			m.moveFindingCursor(step.StepName, 1)
		}
		return m, nil

	case "k", "up":
		if m.showDiff && m.diffOffset > 0 {
			m.diffOffset--
		} else if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.moveFindingCursor(step.StepName, -1)
			}
		}
		return m, nil

	case "g", "home":
		if m.showDiff {
			m.diffOffset = 0
		} else if step := awaitingStep(m.steps); step != nil {
			m.findingCursor[step.StepName] = 0
		}
		return m, nil

	case "G", "end":
		if m.showDiff {
			m.diffOffset = 1<<31 - 1 // large value, renderDiff clamps
		} else if step := awaitingStep(m.steps); step != nil {
			items := m.findingItems(step.StepName)
			if len(items) > 0 {
				m.findingCursor[step.StepName] = len(items) - 1
			}
		}
		return m, nil

	case "ctrl+d":
		if m.showDiff {
			half := (m.height - 15) / 2
			if half < 1 {
				half = 1
			}
			m.diffOffset += half
		} else if step := awaitingStep(m.steps); step != nil {
			half := (m.height - 20) / 3 / 2
			if half < 1 {
				half = 1
			}
			m.moveFindingCursor(step.StepName, half)
		}
		return m, nil

	case "ctrl+u":
		if m.showDiff {
			half := (m.height - 15) / 2
			if half < 1 {
				half = 1
			}
			m.diffOffset -= half
			if m.diffOffset < 0 {
				m.diffOffset = 0
			}
		} else if step := awaitingStep(m.steps); step != nil {
			half := (m.height - 20) / 3 / 2
			if half < 1 {
				half = 1
			}
			m.moveFindingCursor(step.StepName, -half)
		}
		return m, nil

	case " ":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.toggleCurrentFinding(step.StepName)
				m.moveFindingCursor(step.StepName, 1)
			}
		}
		return m, nil

	case "A":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.selectAllFindings(step.StepName)
			}
		}
		return m, nil

	case "N":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.clearAllFindings(step.StepName)
			}
		}
		return m, nil

	case "e":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				if item, ok := m.findingAtCursor(step.StepName); ok && item.ID != "" {
					existing := ""
					if byStep := m.findingInstructions[step.StepName]; byStep != nil {
						existing = byStep[item.ID]
					}
					if item.Source == types.FindingSourceUser {
						existing = item.UserInstructions
					}
					m.editor = newInstructionEditor(step.StepName, item.ID, existing)
				}
			}
		}
		return m, nil
	case "+":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				m.editor = newAddFindingEditor(step.StepName)
			}
		}
		return m, nil
	case "D":
		if !m.showDiff {
			if step := awaitingStep(m.steps); step != nil {
				if item, ok := m.findingAtCursor(step.StepName); ok && item.Source == types.FindingSourceUser {
					m.removeUserFinding(step.StepName, item.ID)
					m.moveFindingCursor(step.StepName, 0)
				}
			}
		}
		return m, nil

	case "a":
		return m, m.respondCmd(types.ActionApprove)
	case "f":
		return m, m.respondCmd(types.ActionFix)
	case "s":
		return m, m.respondCmd(types.ActionSkip)
	case "o":
		if m.run != nil && m.run.PRURL != nil && *m.run.PRURL != "" {
			return m, openBrowserCmd(*m.run.PRURL)
		}
		return m, nil
	case "r":
		if m.rerunPending || !canRerun(m.run) {
			return m, nil
		}
		m.rerunPending = true
		m.rerunRequestID++
		return m, m.rerunCmd(m.rerunRequestID)
	case "x":
		if m.done || m.run == nil {
			return m, nil
		}
		if m.confirmAbort {
			m.confirmAbort = false
			return m, m.cancelRunCmd()
		}
		m.confirmAbort = true
		return m, nil
	}
	return m, nil
}

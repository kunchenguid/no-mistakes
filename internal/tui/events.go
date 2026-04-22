package tui

import (
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (m *Model) applyEvent(event ipc.Event) {
	switch event.Type {
	case ipc.EventRunUpdated, ipc.EventRunCreated:
		m.err = nil
		if event.Status != nil {
			m.run.Status = types.RunStatus(*event.Status)
		}
		if event.PRURL != nil {
			m.run.PRURL = event.PRURL
		}

	case ipc.EventRunCompleted:
		m.err = nil
		if event.Status != nil {
			m.run.Status = types.RunStatus(*event.Status)
		}
		if event.Error != nil {
			m.run.Error = event.Error
		}
		if event.PRURL != nil {
			m.run.PRURL = event.PRURL
		}
		if m.syntheticSteps {
			m.steps = nil
			m.run.Steps = nil
		}
		m.flushPartialLog()
		m.done = true

	case ipc.EventStepStarted:
		m.err = nil
		m.syntheticSteps = false
		if event.StepName != nil {
			m.updateStepStatus(*event.StepName, types.StepStatusRunning)
			m.stepStartTimes[*event.StepName] = time.Now()
		}

	case ipc.EventStepCompleted:
		m.err = nil
		m.syntheticSteps = false
		m.flushPartialLog()
		if event.StepName != nil && event.Status != nil {
			m.updateStepStatus(*event.StepName, types.StepStatus(*event.Status))
		}
		if event.StepName != nil && event.Error != nil {
			m.setStepError(*event.StepName, event.Error)
		}
		// Persist duration so the step continues to display its elapsed time.
		// Prefer the event's execution-only duration; fall back to local timing.
		// For "fixing" status, clear the persisted duration and back-date the
		// start time by the accumulated execution so the live timer continues
		// from where it left off rather than resetting to zero.
		if event.StepName != nil && event.Status != nil && types.StepStatus(*event.Status) == types.StepStatusFixing {
			var accumulated time.Duration
			for _, s := range m.steps {
				if s.StepName == *event.StepName {
					if s.DurationMS != nil {
						accumulated = time.Duration(*s.DurationMS) * time.Millisecond
					} else if startTime, ok := m.stepStartTimes[*event.StepName]; ok {
						accumulated = time.Since(startTime)
					}
					break
				}
			}
			m.setStepDuration(*event.StepName, nil)
			m.stepStartTimes[*event.StepName] = time.Now().Add(-accumulated)
		} else if event.StepName != nil {
			if event.DurationMS != nil {
				m.setStepDuration(*event.StepName, event.DurationMS)
			} else if startTime, ok := m.stepStartTimes[*event.StepName]; ok {
				elapsed := int64(time.Since(startTime).Milliseconds())
				m.setStepDuration(*event.StepName, &elapsed)
			}
		}
		if event.StepName != nil && event.Findings != nil && *event.Findings != "" {
			m.stepFindings[*event.StepName] = *event.Findings
			// Reset diff view when new findings arrive to prevent stale showDiff
			// from a previous step hiding these findings.
			m.showDiff = false
			m.diffOffset = 0
			if event.Status != nil && (types.StepStatus(*event.Status) == types.StepStatusAwaitingApproval || types.StepStatus(*event.Status) == types.StepStatusFixReview) {
				delete(m.findingInstructions, *event.StepName)
				delete(m.addedFindings, *event.StepName)
				m.resetFindingSelection(*event.StepName)
			}
		}
		if event.StepName != nil && event.Diff != nil && *event.Diff != "" {
			m.stepDiffs[*event.StepName] = *event.Diff
			m.showDiff = false
			m.diffOffset = 0
		}

	case ipc.EventLogChunk:
		if event.Content != nil && *event.Content != "" {
			if m.logPartial != "" && len(m.logs) > 0 && m.logs[len(m.logs)-1] == m.logPartial {
				m.logs = m.logs[:len(m.logs)-1]
			}

			text := m.logPartial + *event.Content
			m.logPartial = ""

			if !strings.HasSuffix(text, "\n") {
				idx := strings.LastIndex(text, "\n")
				if idx == -1 {
					m.logPartial = text
					text = ""
				} else {
					m.logPartial = text[idx+1:]
					text = text[:idx+1]
				}
			}

			if text != "" {
				lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
				m.logs = append(m.logs, lines...)
			}
			if m.logPartial != "" {
				m.logs = append(m.logs, m.logPartial)
			}
			if len(m.logs) > 100 {
				m.logs = m.logs[len(m.logs)-100:]
			}
		}
	}
}

func (m *Model) updateStepStatus(name types.StepName, status types.StepStatus) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].Status = status
			return
		}
	}
}

func (m *Model) flushPartialLog() {
	if m.logPartial == "" {
		return
	}
	if len(m.logs) > 0 && m.logs[len(m.logs)-1] == m.logPartial {
		m.logPartial = ""
		return
	}
	m.logs = append(m.logs, m.logPartial)
	m.logPartial = ""
	if len(m.logs) > 100 {
		m.logs = m.logs[len(m.logs)-100:]
	}
}

func (m *Model) setStepDuration(name types.StepName, durationMS *int64) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].DurationMS = durationMS
			return
		}
	}
}

func (m *Model) setStepError(name types.StepName, errMsg *string) {
	for i := range m.steps {
		if m.steps[i].StepName == name {
			m.steps[i].Error = errMsg
			return
		}
	}
}

// stepsWithRunningElapsed returns a copy of m.steps with DurationMS set on
// running/fixing steps based on their recorded start times.
func (m Model) stepsWithRunningElapsed() []ipc.StepResultInfo {
	steps := make([]ipc.StepResultInfo, len(m.steps))
	copy(steps, m.steps)
	for i := range steps {
		if steps[i].DurationMS != nil {
			continue
		}
		switch steps[i].Status {
		case types.StepStatusRunning, types.StepStatusFixing,
			types.StepStatusAwaitingApproval, types.StepStatusFixReview:
			if startTime, ok := m.stepStartTimes[steps[i].StepName]; ok {
				elapsed := int64(time.Since(startTime).Milliseconds())
				steps[i].DurationMS = &elapsed
			}
		}
	}
	return steps
}

package pipeline

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// stepLogWriter owns the mutable state behind one step's user-visible and
// file-only log callbacks. A review fleet calls these callbacks from four
// concurrent adapter goroutines, so keeping the framing and active PID set in
// the executor (rather than in ReviewStep or each adapter) makes both output
// ordering and lifecycle cleanup race-safe.
type stepLogWriter struct {
	mu               sync.Mutex
	file             *os.File
	emit             func(string)
	touch            func(string)
	lastChunkNewline bool
	lastActivityAt   time.Time
	activePIDs       map[int]struct{}
}

func newStepLogWriter(file *os.File, emit func(string), touch func(string)) *stepLogWriter {
	return &stepLogWriter{
		file:             file,
		emit:             emit,
		touch:            touch,
		lastChunkNewline: true,
		activePIDs:       make(map[int]struct{}),
	}
}

func (l *stepLogWriter) writeLine(text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeLineLocked(text)
}

func (l *stepLogWriter) writeLineLocked(text string) {
	if text != "" {
		prefix := ""
		if !l.lastChunkNewline {
			prefix = "\n"
		}
		text = prefix + strings.TrimRight(text, "\n") + "\n\n"
		l.lastChunkNewline = true
	}
	l.emitAndWriteLocked(text)
	l.touchLocked(text, true)
}

func (l *stepLogWriter) writeChunk(text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if text != "" {
		l.lastChunkNewline = strings.HasSuffix(text, "\n")
	}
	l.emitAndWriteLocked(text)
	l.touchLocked(text, strings.Contains(text, "\n"))
}

func (l *stepLogWriter) writeFileOnly(text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_, _ = fmt.Fprintln(l.file, text)
	}
	l.touchLocked(text, true)
}

func (l *stepLogWriter) emitAndWriteLocked(text string) {
	if l.emit != nil {
		l.emit(text)
	}
	if l.file != nil {
		_, _ = fmt.Fprint(l.file, text)
	}
}

func (l *stepLogWriter) touchLocked(text string, force bool) {
	if l.touch == nil || stepActivityFromLog(text) == "" {
		return
	}
	now := time.Now()
	if !force && !l.lastActivityAt.IsZero() && now.Sub(l.lastActivityAt) < stepActivityThrottleInterval {
		return
	}
	l.lastActivityAt = now
	l.touch(stepActivityFromLog(text))
}

// lifecycle updates the aggregate PID state before handing the event to the
// executor's DB callback. An exit only clears agent_pid after the last active
// PID is gone; this prevents one reviewer finishing from hiding three still
// running reviewers in status output.
func (l *stepLogWriter) lifecycle(event agent.LifecycleEvent, persist func(string, *int)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	text := event.Message
	if text == "" {
		text = fmt.Sprintf("%s %s", event.Agent, event.Phase)
	}
	switch event.Phase {
	case agent.LifecyclePhaseStart:
		if event.PID > 0 {
			l.activePIDs[event.PID] = struct{}{}
		}
		persist(text, l.activePIDLocked())
	case agent.LifecyclePhaseExit:
		if event.PID > 0 {
			delete(l.activePIDs, event.PID)
		}
		persist(text, l.activePIDLocked())
	default:
		l.touchLocked(text, true)
		persist(text, nil)
	}
	l.writeLineLocked(text)
}

func (l *stepLogWriter) activePIDLocked() *int {
	if len(l.activePIDs) == 0 {
		return nil
	}
	pids := make([]int, 0, len(l.activePIDs))
	for pid := range l.activePIDs {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	pid := pids[0]
	return &pid
}

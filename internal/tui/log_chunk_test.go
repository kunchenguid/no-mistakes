package tui

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestModel_ApplyEvent_LogChunk_PartialLines(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// Simulate streaming chunks without trailing newlines (like OpenCode SSE deltas).
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("hello "),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected partial line to be visible, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "hello " {
		t.Fatalf("expected visible partial %q, got %q", "hello ", m.logs[0])
	}
	if m.logPartial != "hello " {
		t.Fatalf("expected buffered partial %q, got %q", "hello ", m.logPartial)
	}

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("world"),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected updated partial line to remain visible, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "hello world" {
		t.Fatalf("expected visible partial %q, got %q", "hello world", m.logs[0])
	}
	if m.logPartial != "hello world" {
		t.Fatalf("expected buffered partial %q, got %q", "hello world", m.logPartial)
	}

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("\n"),
	})

	// "hello world" should be a single log line, not three separate lines.
	if len(m.logs) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", m.logs[0])
	}
}

func TestModel_ApplyEvent_LogChunk_MixedPartialAndComplete(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// A chunk that has a complete line and a partial one.
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("line1\npartial"),
	})

	// Should have committed "line1" and kept the partial visible.
	if len(m.logs) != 2 {
		t.Fatalf("expected 2 visible lines, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "line1" {
		t.Errorf("expected %q, got %q", "line1", m.logs[0])
	}
	if m.logs[1] != "partial" {
		t.Errorf("expected %q, got %q", "partial", m.logs[1])
	}
	if m.logPartial != "partial" {
		t.Fatalf("expected buffered partial %q, got %q", "partial", m.logPartial)
	}

	// Completing the partial line.
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr(" end\n"),
	})

	if len(m.logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[1] != "partial end" {
		t.Errorf("expected %q, got %q", "partial end", m.logs[1])
	}
}

func TestModel_ApplyEvent_LogChunk_FlushesPartialOnStepCompleted(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("last line without newline"),
	})

	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepName("review")),
		Status:   ptr(string(types.StepStatusCompleted)),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "last line without newline" {
		t.Fatalf("expected flushed log line, got %q", m.logs[0])
	}
	if m.logPartial != "" {
		t.Fatalf("expected partial log buffer to be cleared, got %q", m.logPartial)
	}

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("next line\n"),
	})

	if len(m.logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[1] != "next line" {
		t.Fatalf("expected independent next line, got %q", m.logs[1])
	}
}

func TestModel_ApplyEvent_LogChunk_FlushesPartialOnRunCompleted(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("trailing output"),
	})

	m.applyEvent(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		Status: ptr(string(types.RunCompleted)),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "trailing output" {
		t.Fatalf("expected flushed log line, got %q", m.logs[0])
	}
	if m.logPartial != "" {
		t.Fatalf("expected partial log buffer to be cleared, got %q", m.logPartial)
	}
}

func TestModel_ApplyEvent_LogChunk_BlankLineSeparators(t *testing.T) {
	// The executor's Log callback formats discrete messages as "text\n\n",
	// with a leading \n only when flushing an unterminated streaming partial.
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// Streaming agent text without trailing newline (partial).
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("streaming text"),
	})
	// Discrete message after unterminated stream: leading \n flushes partial.
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("\ncommitted agent fixes\n\n"),
	})
	// Consecutive discrete message: no leading \n (previous ended with \n\n).
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("reviewing changes...\n\n"),
	})

	// Exactly one blank line between each entry.
	want := []string{"streaming text", "committed agent fixes", "", "reviewing changes...", ""}
	if len(m.logs) != len(want) {
		t.Fatalf("expected %d log entries, got %d: %v", len(want), len(m.logs), m.logs)
	}
	for i, w := range want {
		if m.logs[i] != w {
			t.Errorf("logs[%d] = %q, want %q", i, m.logs[i], w)
		}
	}
}

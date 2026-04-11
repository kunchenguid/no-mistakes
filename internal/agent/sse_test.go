package agent

import (
	"strings"
	"testing"
)

func TestParseSSE_SimpleEvent(t *testing.T) {
	input := "data: hello world\n\n"
	var events []sseEvent
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "hello world" {
		t.Errorf("expected data 'hello world', got %q", events[0].Data)
	}
	if events[0].Name != "" {
		t.Errorf("expected empty name, got %q", events[0].Name)
	}
}

func TestParseSSE_NamedEvent(t *testing.T) {
	input := "event: text\ndata: content here\n\n"
	var events []sseEvent
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Name != "text" {
		t.Errorf("expected name 'text', got %q", events[0].Name)
	}
	if events[0].Data != "content here" {
		t.Errorf("expected data 'content here', got %q", events[0].Data)
	}
}

func TestParseSSE_MultiLineData(t *testing.T) {
	input := "data: line1\ndata: line2\ndata: line3\n\n"
	var events []sseEvent
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "line1\nline2\nline3" {
		t.Errorf("expected multi-line data, got %q", events[0].Data)
	}
}

func TestParseSSE_MultipleEvents(t *testing.T) {
	input := "data: first\n\ndata: second\n\ndata: third\n\n"
	var events []sseEvent
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Data != "first" {
		t.Errorf("event[0] data: expected 'first', got %q", events[0].Data)
	}
	if events[2].Data != "third" {
		t.Errorf("event[2] data: expected 'third', got %q", events[2].Data)
	}
}

func TestParseSSE_StopOnFalse(t *testing.T) {
	input := "data: one\n\ndata: two\n\ndata: three\n\n"
	count := 0
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		count++
		return count < 2 // stop after second event
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected handler called 2 times, got %d", count)
	}
}

func TestParseSSE_CRLFSeparators(t *testing.T) {
	input := "event: msg\r\ndata: crlf content\r\n\r\n"
	var events []sseEvent
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Name != "msg" {
		t.Errorf("expected name 'msg', got %q", events[0].Name)
	}
	if events[0].Data != "crlf content" {
		t.Errorf("expected data 'crlf content', got %q", events[0].Data)
	}
}

func TestParseSSE_EmptyInput(t *testing.T) {
	var events []sseEvent
	err := parseSSE(strings.NewReader(""), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestParseSSE_TrailingEventWithoutNewline(t *testing.T) {
	input := "data: trailing"
	var events []sseEvent
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (trailing flush), got %d", len(events))
	}
	if events[0].Data != "trailing" {
		t.Errorf("expected data 'trailing', got %q", events[0].Data)
	}
}

func TestParseSSE_IgnoresComments(t *testing.T) {
	input := ": this is a comment\ndata: real data\n\n"
	var events []sseEvent
	err := parseSSE(strings.NewReader(input), func(ev sseEvent) bool {
		events = append(events, ev)
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "real data" {
		t.Errorf("expected data 'real data', got %q", events[0].Data)
	}
}

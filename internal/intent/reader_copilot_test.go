package intent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCopilotFixture builds a Copilot-style session-state directory inside a
// fake $HOME and returns the home path. The session id is fixed so tests can
// assert on it.
func writeCopilotFixture(t *testing.T, sessionID string, lines []string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".copilot", "session-state", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestCopilotReader_DiscoversAndLoadsRealMessages(t *testing.T) {
	repoCWD := t.TempDir()
	fooPath := filepath.Join(repoCWD, "internal", "foo.go")
	home := writeCopilotFixture(t, "sess-1", []string{
		`{"type":"session.start","data":{"context":{"cwd":` + jsonString(t, repoCWD) + `},"startTime":"2026-04-18T02:15:37.000Z"},"id":"e0","timestamp":"2026-04-18T02:15:37.000Z"}`,
		`{"type":"user.message","data":{"content":"please add a foo helper to internal/foo.go","transformedContent":"<agent_instructions>ignore me</agent_instructions>please add a foo helper"},"id":"e1","timestamp":"2026-04-18T02:15:38.000Z"}`,
		`{"type":"assistant.message","data":{"content":"got it","reasoningText":"thinking","toolRequests":[{"name":"edit","arguments":{"path":` + jsonString(t, fooPath) + `}}],"outputTokens":12},"id":"e2","timestamp":"2026-04-18T02:15:39.000Z"}`,
		// Synthetic skill-context user message should be skipped.
		`{"type":"user.message","data":{"content":"<skill-context name=\"repo-context\">base dir...</skill-context>"},"id":"e3","timestamp":"2026-04-18T02:15:40.000Z"}`,
		// Non-message events should be skipped but still advance LastMsgKey.
		`{"type":"tool.execution_complete","data":{"toolCallId":"t1"},"id":"e4","timestamp":"2026-04-18T02:15:41.000Z"}`,
	})

	r := NewCopilotReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Now().Add(-24 * time.Hour),
		WindowEnd:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("discovered %d sessions, want 1: %+v", len(sessions), sessions)
	}
	s := sessions[0]
	if s.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", s.SessionID)
	}
	if canonicalPath(s.CWD) != canonicalPath(repoCWD) {
		t.Errorf("CWD = %q, want %q", s.CWD, repoCWD)
	}

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (synthetic + tool event skipped): %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[0].Role != RoleUser || !strings.Contains(s.Messages[0].Text, "foo helper") {
		t.Errorf("first message wrong: %+v", s.Messages[0])
	}
	// transformedContent must not leak into the user message text.
	if strings.Contains(s.Messages[0].Text, "agent_instructions") {
		t.Errorf("transformedContent leaked into user text: %q", s.Messages[0].Text)
	}
	if s.Messages[1].Role != RoleAssistant {
		t.Errorf("second message not assistant: %+v", s.Messages[1])
	}
	foundPath := false
	for _, p := range s.Messages[1].FilePaths {
		if strings.HasSuffix(filepath.ToSlash(p), "internal/foo.go") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("expected tool path captured, got %v", s.Messages[1].FilePaths)
	}
	if strings.Contains(s.Messages[1].Text, "thinking") {
		t.Error("reasoningText leaked into assistant text")
	}
	if s.LastMsgKey != "e4" {
		t.Errorf("LastMsgKey = %q, want e4 (last id in file)", s.LastMsgKey)
	}
}

func TestCopilotReader_FiltersByCWD(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	home := writeCopilotFixture(t, "sess-a", []string{
		`{"type":"session.start","data":{"context":{"cwd":` + jsonString(t, repoA) + `},"startTime":"2026-04-18T02:15:37.000Z"},"id":"e0","timestamp":"2026-04-18T02:15:37.000Z"}`,
		`{"type":"user.message","data":{"content":"hi"},"id":"e1","timestamp":"2026-04-18T02:15:38.000Z"}`,
	})

	r := NewCopilotReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoB,
		WindowStart: time.Now().Add(-24 * time.Hour),
		WindowEnd:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions for unrelated cwd, got %d", len(sessions))
	}
}

func TestCopilotReader_TimeWindow(t *testing.T) {
	repoCWD := t.TempDir()
	home := writeCopilotFixture(t, "sess-old", []string{
		`{"type":"session.start","data":{"context":{"cwd":` + jsonString(t, repoCWD) + `}},"id":"e0","timestamp":"2020-01-01T00:00:00.000Z"}`,
		`{"type":"user.message","data":{"content":"hi"},"id":"e1","timestamp":"2020-01-01T00:00:01.000Z"}`,
	})

	r := NewCopilotReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// The fixture's events.jsonl was just written, so its mod time is "now",
	// which is far outside the 2020 window: it must be excluded.
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions outside window, got %d", len(sessions))
	}
}

func TestCopilotReader_NoHomeNoCrash(t *testing.T) {
	r := NewCopilotReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:   t.TempDir(), // exists but no .copilot/session-state/
		OriginCWD: "/somewhere",
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions when session-state dir missing, got %d", len(sessions))
	}
}

func TestCopilotReader_SkipsSessionWithoutStartEvent(t *testing.T) {
	repoCWD := t.TempDir()
	home := writeCopilotFixture(t, "sess-nostart", []string{
		`{"type":"user.message","data":{"content":"hi"},"id":"e1","timestamp":"2026-04-18T02:15:38.000Z"}`,
	})

	r := NewCopilotReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Now().Add(-24 * time.Hour),
		WindowEnd:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions without session.start metadata, got %d", len(sessions))
	}
}

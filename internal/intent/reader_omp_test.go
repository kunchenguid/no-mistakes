package intent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOMPReader_DiscoverAndLoadFromOMPRoot proves the OMP reader scans
// ~/.omp/agent/sessions (not ~/.pi) and reuses the shared pi-format parsing:
// session metadata, streamed message_update dropping, and agent_end batches.
func TestOMPReader_DiscoverAndLoadFromOMPRoot(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".omp", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_omp-session.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"omp-session","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_update","id":"u1","timestamp":"2026-04-18T02:15:38.000Z","message":{"role":"assistant","responseId":"r1"},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"partial"}}`,
		`{"type":"message_end","id":"u2","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"user","content":"fix internal/omp.go"}}`,
		`{"type":"turn_end","id":"u3","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"assistant","content":[{"type":"text","text":"updated it"},{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/omp.go"}}]}}`,
		`{"type":"agent_end","id":"u4","timestamp":"2026-04-18T02:15:41.000Z","messages":[{"role":"user","content":"also update docs/omp.md"},{"role":"assistant","content":[{"type":"text","text":"updated docs"}]}]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := readerByName(t, AllReaders(nil), "omp")
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.AgentName != "omp" {
		t.Errorf("AgentName = %q, want omp", s.AgentName)
	}
	if s.SessionID != "omp-session" {
		t.Errorf("SessionID = %q, want omp-session", s.SessionID)
	}
	if canonicalPath(s.CWD) != canonicalPath(repoCWD) {
		t.Errorf("CWD = %q, want %q", s.CWD, repoCWD)
	}

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("load: %v", err)
	}
	msgs := s.Messages
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || !strings.Contains(msgs[0].Text, "internal/omp.go") {
		t.Errorf("message_end user message wrong: %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || !strings.Contains(msgs[1].Text, "updated it") {
		t.Errorf("turn_end assistant message wrong: %+v", msgs[1])
	}
	if len(msgs[1].FilePaths) != 1 || msgs[1].FilePaths[0] != "internal/omp.go" {
		t.Errorf("turn_end tool paths = %v, want [internal/omp.go]", msgs[1].FilePaths)
	}
	if msgs[2].Role != RoleUser || !strings.Contains(msgs[2].Text, "docs/omp.md") {
		t.Errorf("agent_end user message wrong: %+v", msgs[2])
	}
	if msgs[3].Role != RoleAssistant || !strings.Contains(msgs[3].Text, "updated docs") {
		t.Errorf("agent_end assistant message wrong: %+v", msgs[3])
	}
	if s.LastMsgKey != "u4" {
		t.Errorf("LastMsgKey = %q, want u4", s.LastMsgKey)
	}
}

// TestOMPReader_IgnoresPiRoot ensures the OMP reader does not read the Pi
// session directory, keeping the two backends' transcripts separate.
func TestOMPReader_IgnoresPiRoot(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	piDir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(piDir, "2026-04-18T02-15-37-407Z_pi-session.jsonl")
	line := `{"type":"session","version":3,"id":"pi-session","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + "}\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := NewOMPReader().Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("omp reader picked up Pi sessions: %+v", sessions)
	}
}

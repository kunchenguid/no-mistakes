package intent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiReader_DiscoverAndLoad(t *testing.T) {
	repoCWD := t.TempDir()
	home := writePiFixture(t, repoCWD)

	r := readerByName(t, AllReaders(nil), "pi")
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
	if s.AgentName != "pi" {
		t.Errorf("AgentName = %q, want pi", s.AgentName)
	}
	if s.SessionID != "session-1" {
		t.Errorf("SessionID = %q, want session-1", s.SessionID)
	}
	if canonicalPath(s.CWD) != canonicalPath(repoCWD) {
		t.Errorf("CWD = %q, want %q", s.CWD, repoCWD)
	}

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Messages) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[0].Role != RoleUser || !strings.Contains(s.Messages[0].Text, "foo helper") {
		t.Errorf("first message wrong: %+v", s.Messages[0])
	}
	if s.Messages[1].Role != RoleAssistant || !strings.Contains(s.Messages[1].Text, "I'll edit") {
		t.Errorf("second message wrong: %+v", s.Messages[1])
	}
	if strings.Contains(s.Messages[1].Text, "private chain of thought") {
		t.Errorf("assistant thinking leaked into text: %q", s.Messages[1].Text)
	}
	foundToolPath := false
	for _, p := range s.Messages[1].FilePaths {
		if p == "internal/foo.go" {
			foundToolPath = true
		}
	}
	if !foundToolPath {
		t.Errorf("expected tool-call path in assistant FilePaths, got %v", s.Messages[1].FilePaths)
	}
	if s.Messages[2].Role != RoleAssistant || s.Messages[2].Text != "" {
		t.Errorf("third message should be a path-only assistant tool call, got %+v", s.Messages[2])
	}
	foundCamelPath := false
	for _, p := range s.Messages[2].FilePaths {
		if p == "internal/bar.go" {
			foundCamelPath = true
		}
	}
	if !foundCamelPath {
		t.Errorf("expected camelCase filePath in path-only tool call, got %v", s.Messages[2].FilePaths)
	}
	foundCommandPath := false
	for _, p := range s.Messages[3].FilePaths {
		if p == "internal/baz.go" {
			foundCommandPath = true
		}
	}
	if !foundCommandPath {
		t.Errorf("expected shell command path in path-only tool call, got %v", s.Messages[3].FilePaths)
	}
	if s.LastMsgKey != "m6" {
		t.Errorf("LastMsgKey = %q, want m6", s.LastMsgKey)
	}
}

func TestPiReader_LoadsPiEventStreamRecords(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-events.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-events","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_update","id":"u1","timestamp":"2026-04-18T02:15:38.000Z","message":{"role":"assistant","responseId":"r1"},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"partial"}}`,
		`{"type":"message_end","id":"u2","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"user","content":"fix internal/pi.go"}}`,
		`{"type":"turn_end","id":"u3","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"assistant","content":[{"type":"text","text":"updated it"},{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/pi.go"}}]}}`,
		`{"type":"agent_end","id":"u4","timestamp":"2026-04-18T02:15:41.000Z","messages":[{"role":"user","content":"also update docs/pi.md"},{"role":"assistant","content":[{"type":"text","text":"updated docs"}]}]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewPiReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}

	msgs := sessions[0].Messages
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || !strings.Contains(msgs[0].Text, "internal/pi.go") {
		t.Errorf("message_end user message wrong: %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || !strings.Contains(msgs[1].Text, "updated it") {
		t.Errorf("turn_end assistant message wrong: %+v", msgs[1])
	}
	if len(msgs[1].FilePaths) != 1 || msgs[1].FilePaths[0] != "internal/pi.go" {
		t.Errorf("turn_end tool paths = %v, want [internal/pi.go]", msgs[1].FilePaths)
	}
	if msgs[2].Role != RoleUser || !strings.Contains(msgs[2].Text, "docs/pi.md") {
		t.Errorf("agent_end user message wrong: %+v", msgs[2])
	}
	if msgs[3].Role != RoleAssistant || !strings.Contains(msgs[3].Text, "updated docs") {
		t.Errorf("agent_end assistant message wrong: %+v", msgs[3])
	}
	if sessions[0].LastMsgKey != "u4" {
		t.Errorf("LastMsgKey = %q, want u4", sessions[0].LastMsgKey)
	}
}

func TestPiReader_IgnoresStreamingMessageUpdates(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-updates.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-updates","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_update","id":"u1","timestamp":"2026-04-18T02:15:38.000Z","message":{"role":"assistant","content":[{"type":"text","text":"updated"}]}}`,
		`{"type":"message_update","id":"u2","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"assistant","content":[{"type":"text","text":"updated internal"}]}}`,
		`{"type":"turn_end","id":"u3","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"assistant","content":[{"type":"text","text":"updated internal/pi.go"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewPiReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}

	msgs := sessions[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleAssistant || msgs[0].Text != "updated internal/pi.go" {
		t.Errorf("message wrong: %+v", msgs[0])
	}
	if sessions[0].LastMsgKey != "u3" {
		t.Errorf("LastMsgKey = %q, want u3", sessions[0].LastMsgKey)
	}
}

func TestPiReader_DeduplicatesCompletedEventsByResponseID(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-completed-dedupe.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-completed-dedupe","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_end","id":"u1","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"assistant","responseId":"r1","content":[{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/intent/reader_pi.go"}}]}}`,
		`{"type":"turn_end","id":"u2","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"assistant","responseId":"r1","content":[{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/intent/reader_pi.go"}}]}}`,
		`{"type":"message_end","id":"u3","timestamp":"2026-04-18T02:15:41.000Z","message":{"role":"assistant","responseId":"r2","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"turn_end","id":"u4","timestamp":"2026-04-18T02:15:42.000Z","message":{"role":"assistant","responseId":"r2","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewPiReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}

	msgs := sessions[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if len(msgs[0].FilePaths) != 1 || msgs[0].FilePaths[0] != "internal/intent/reader_pi.go" {
		t.Errorf("first message paths = %v, want [internal/intent/reader_pi.go]", msgs[0].FilePaths)
	}
	if msgs[1].Role != RoleAssistant || msgs[1].Text != "done" {
		t.Errorf("second message wrong: %+v", msgs[1])
	}
	if sessions[0].LastMsgKey != "u4" {
		t.Errorf("LastMsgKey = %q, want u4", sessions[0].LastMsgKey)
	}
}

func TestPiReader_DeduplicatesCompletedEventsByMessageID(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-completed-message-id-dedupe.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-completed-message-id-dedupe","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_end","id":"u1","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"assistant","id":"m1","content":[{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/intent/reader_pi.go"}}]}}`,
		`{"type":"turn_end","id":"u2","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"assistant","id":"m1","content":[{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/intent/reader_pi.go"}}]}}`,
		`{"type":"message_end","id":"u3","timestamp":"2026-04-18T02:15:41.000Z","message":{"role":"assistant","id":"m2","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"turn_end","id":"u4","timestamp":"2026-04-18T02:15:42.000Z","message":{"role":"assistant","id":"m2","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewPiReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}

	msgs := sessions[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if len(msgs[0].FilePaths) != 1 || msgs[0].FilePaths[0] != "internal/intent/reader_pi.go" {
		t.Errorf("first message paths = %v, want [internal/intent/reader_pi.go]", msgs[0].FilePaths)
	}
	if msgs[1].Role != RoleAssistant || msgs[1].Text != "done" {
		t.Errorf("second message wrong: %+v", msgs[1])
	}
	if sessions[0].LastMsgKey != "u4" {
		t.Errorf("LastMsgKey = %q, want u4", sessions[0].LastMsgKey)
	}
}

func TestPiReader_DeduplicatesAgentEndMessages(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-dedupe.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-dedupe","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_end","id":"u1","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"user","content":"fix internal/pi.go"}}`,
		`{"type":"turn_end","id":"u2","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"assistant","content":[{"type":"text","text":"updated it"}]}}`,
		`{"type":"agent_end","id":"u3","timestamp":"2026-04-18T02:15:41.000Z","messages":[{"role":"user","content":"fix internal/pi.go"},{"role":"assistant","content":[{"type":"text","text":"updated it"}]}]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewPiReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}

	msgs := sessions[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || msgs[0].Text != "fix internal/pi.go" {
		t.Errorf("first message wrong: %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || msgs[1].Text != "updated it" {
		t.Errorf("second message wrong: %+v", msgs[1])
	}
}

func TestPiReader_PreservesRepeatedLiveMessages(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-repeat.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-repeat","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_end","id":"u1","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"user","content":"yes"}}`,
		`{"type":"message_end","id":"u2","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"user","content":"yes"}}`,
		`{"type":"turn_end","id":"u3","timestamp":"2026-04-18T02:15:41.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"turn_end","id":"u4","timestamp":"2026-04-18T02:15:42.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewPiReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}

	msgs := sessions[0].Messages
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	for i, want := range []struct {
		role Role
		text string
	}{
		{RoleUser, "yes"},
		{RoleUser, "yes"},
		{RoleAssistant, "done"},
		{RoleAssistant, "done"},
	} {
		if msgs[i].Role != want.role || msgs[i].Text != want.text {
			t.Errorf("message %d = %+v, want role %s text %q", i, msgs[i], want.role, want.text)
		}
	}
}

func TestPiReader_LoadsOversizedAgentEndRecord(t *testing.T) {
	// Keep this well above bufio.Scanner's default 64 KiB token limit without
	// making Windows CI spend excessive time writing and parsing the fixture.
	const oversizedPayloadSize = 1024 * 1024

	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-large.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-large","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"message_end","id":"u1","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"user","content":"fix internal/pi.go"}}`,
		`{"type":"agent_end","id":"u2","timestamp":"2026-04-18T02:15:41.000Z","messages":[{"role":"toolResult","content":"` + strings.Repeat("x", oversizedPayloadSize) + `"}]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewPiReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}

	msgs := sessions[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || msgs[0].Text != "fix internal/pi.go" {
		t.Errorf("message wrong: %+v", msgs[0])
	}
	if sessions[0].LastMsgKey != "u2" {
		t.Errorf("LastMsgKey = %q, want u2", sessions[0].LastMsgKey)
	}
}

func readerByName(t *testing.T, readers []Reader, name string) Reader {
	t.Helper()
	for _, r := range readers {
		if r.Name() == name {
			return r
		}
	}
	t.Fatalf("reader %q not found", name)
	return nil
}

func writePiFixture(t *testing.T, repoCWD string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-04-18T02-15-37-407Z_session-1.jsonl")
	lines := []string{
		`{"type":"session","version":3,"id":"session-1","timestamp":"2026-04-18T02:15:37.407Z","cwd":` + jsonString(t, repoCWD) + `}`,
		`{"type":"model_change","id":"meta-1","timestamp":"2026-04-18T02:15:37.500Z","modelId":"gpt-5.5"}`,
		`{"type":"message","id":"m1","timestamp":"2026-04-18T02:15:38.000Z","message":{"role":"user","content":[{"type":"text","text":"please add a foo helper to internal/foo.go"}]}}`,
		`{"type":"message","id":"m2","timestamp":"2026-04-18T02:15:39.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private chain of thought"},{"type":"text","text":"I'll edit internal/foo.go"},{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/foo.go"}}]}}`,
		`{"type":"message","id":"m3","timestamp":"2026-04-18T02:15:40.000Z","message":{"role":"toolResult","toolName":"edit","content":[{"type":"text","text":"tool output should not become transcript text"}]}}`,
		`{"type":"message","id":"m4","timestamp":"2026-04-18T02:15:41.000Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"filePath":"internal/bar.go"}}]}}`,
		`{"type":"message","id":"m5","timestamp":"2026-04-18T02:15:42.000Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"gofmt -w internal/baz.go"}}]}}`,
		`{"type":"message","id":"m6","timestamp":"2026-04-18T02:15:43.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"only thinking should be skipped"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

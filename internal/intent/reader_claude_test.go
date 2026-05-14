package intent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeClaudeFixture builds a minimal Claude-style .jsonl transcript inside
// a fake $HOME and returns the home path.
func writeClaudeFixture(t *testing.T, repoCWD string, lines []string) string {
	t.Helper()
	home := t.TempDir()
	encoded := claudeProjectDirName(repoCWD)
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-uuid-1.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaudeProjectDirNameIsPathSafe(t *testing.T) {
	name := claudeProjectDirName(`C:\Users\runner\work\repo`)
	for _, sep := range []string{`/`, `\`, `:`} {
		if strings.Contains(name, sep) {
			t.Fatalf("claudeProjectDirName() = %q, contains %q", name, sep)
		}
	}
}

func TestClaudeReader_DiscoversAndLoadsRealMessages(t *testing.T) {
	repoCWD := t.TempDir()
	home := writeClaudeFixture(t, repoCWD, []string{
		`{"type":"user","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please add a foo helper to internal/foo.go"}}`,
		`{"type":"assistant","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"text","text":"got it"},{"type":"tool_use","name":"Edit","input":{"file_path":` + jsonString(t, filepath.Join(repoCWD, "internal", "foo.go")) + `,"old_string":"x","new_string":"y"}}]}}`,
		`{"type":"assistant","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:38.500Z","uuid":"u2b","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"filePath":` + jsonString(t, filepath.Join(repoCWD, "internal", "bar.go")) + `}}]}}`,
		// Synthetic user text should be skipped.
		`{"type":"user","isMeta":true,"cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:39.000Z","uuid":"u3","sessionId":"s1","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		// Attachments should be skipped.
		`{"type":"attachment","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:40.000Z","uuid":"u4","attachment":{"type":"hook"}}`,
	})

	r := NewClaudeReader()
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
	if s.SessionID != "session-uuid-1" {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if canonicalPath(s.CWD) != canonicalPath(repoCWD) {
		t.Errorf("CWD = %q, want %q", s.CWD, repoCWD)
	}

	if err := r.Load(context.Background(), s); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (synthetic + attachment skipped)", len(s.Messages))
	}
	if s.Messages[0].Role != RoleUser || !strings.Contains(s.Messages[0].Text, "foo helper") {
		t.Errorf("first message wrong: %+v", s.Messages[0])
	}
	if s.Messages[1].Role != RoleAssistant {
		t.Errorf("second message not assistant: %+v", s.Messages[1])
	}
	// Tool use file path should land on FilePaths, NOT in the text.
	foundPath := false
	for _, p := range s.Messages[1].FilePaths {
		if strings.HasSuffix(filepath.ToSlash(p), "internal/foo.go") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("expected tool_use file_path captured, got %v", s.Messages[1].FilePaths)
	}
	foundCamelPath := false
	for _, p := range s.Messages[2].FilePaths {
		if strings.HasSuffix(filepath.ToSlash(p), "internal/bar.go") {
			foundCamelPath = true
		}
	}
	if !foundCamelPath {
		t.Errorf("expected tool_use filePath captured, got %v", s.Messages[2].FilePaths)
	}
	if strings.Contains(s.Messages[1].Text, "old_string") {
		t.Error("tool_use input leaked into assistant text")
	}
	if s.LastMsgKey != "u4" {
		t.Errorf("LastMsgKey = %q, want u4 (last uuid in file)", s.LastMsgKey)
	}
}

func TestClaudeReader_FiltersByCWD(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()

	home := writeClaudeFixture(t, repoA, []string{
		`{"type":"user","cwd":` + jsonString(t, repoA) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"hi"}}`,
	})

	r := NewClaudeReader()
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

func TestClaudeReader_TimeWindow(t *testing.T) {
	repoCWD := t.TempDir()
	home := writeClaudeFixture(t, repoCWD, []string{
		`{"type":"user","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"hi"}}`,
	})

	r := NewClaudeReader()
	// Window in the distant past should exclude the (just-written) fixture.
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions outside window, got %d", len(sessions))
	}
}

func TestClaudeReader_NoHomeNoCrash(t *testing.T) {
	r := NewClaudeReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:   t.TempDir(), // exists but no .claude/projects/
		OriginCWD: "/somewhere",
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions when projects dir missing, got %d", len(sessions))
	}
}

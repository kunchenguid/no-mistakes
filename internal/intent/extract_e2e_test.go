package intent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtract_EndToEndWithClaudeFixture(t *testing.T) {
	repoCWD := t.TempDir()
	home := writeClaudeFixture(t, repoCWD, []string{
		`{"type":"user","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please rewrite internal/foo.go to add a Bar() function"}}`,
		`{"type":"assistant","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + jsonString(t, filepath.Join(repoCWD, "internal", "foo.go")) + `}}]}}`,
	})

	fa := &fakeAgent{output: `{"summary": "user wanted Bar() helper in internal/foo.go"}`}

	got, err := Extract(context.Background(), ExtractParams{
		HomeDir:    home,
		OriginCWD:  repoCWD,
		DiffFiles:  []string{"internal/foo.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		SlackDays:  3,
		Threshold:  0.2,
		Readers:    AllReaders(nil),
		Cache:      NewMemCache(),
		Summarizer: NewAgentSummarizer(fa, ""),
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.AgentName != "claude" {
		t.Errorf("AgentName = %q", got.AgentName)
	}
	if got.Summary != "user wanted Bar() helper in internal/foo.go" {
		t.Errorf("summary = %q", got.Summary)
	}
}

func TestExtract_EndToEndWithPiFixture(t *testing.T) {
	repoCWD := t.TempDir()
	home := writePiFixture(t, repoCWD)

	fa := &fakeAgent{output: `{"summary": "user wanted foo helper changes in internal/foo.go"}`}

	got, err := Extract(context.Background(), ExtractParams{
		HomeDir:    home,
		OriginCWD:  repoCWD,
		DiffFiles:  []string{"internal/foo.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		SlackDays:  3,
		Threshold:  0.2,
		Readers:    AllReaders(nil),
		Cache:      NewMemCache(),
		Summarizer: NewAgentSummarizer(fa, ""),
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.AgentName != "pi" {
		t.Errorf("AgentName = %q", got.AgentName)
	}
	if got.SessionID != "session-1" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.Summary != "user wanted foo helper changes in internal/foo.go" {
		t.Errorf("summary = %q", got.Summary)
	}
	if !strings.Contains(fa.lastPrompt, "please add a foo helper to internal/foo.go") {
		t.Errorf("prompt should include Pi user text, got %q", fa.lastPrompt)
	}
	if strings.Contains(fa.lastPrompt, "private chain of thought") || strings.Contains(fa.lastPrompt, "tool output should not become transcript text") {
		t.Errorf("prompt leaked non-user-intent Pi content: %q", fa.lastPrompt)
	}
}

package intent

import (
	"context"
	"testing"
	"time"
)

func TestExtract_EndToEndWithClaudeFixture(t *testing.T) {
	repoCWD := t.TempDir()
	home := writeClaudeFixture(t, repoCWD, []string{
		`{"type":"user","cwd":"` + repoCWD + `","timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please rewrite internal/foo.go to add a Bar() function"}}`,
		`{"type":"assistant","cwd":"` + repoCWD + `","timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"` + repoCWD + `/internal/foo.go"}}]}}`,
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

package intent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBuildSummaryTranscript_WindowDoesNotChangeSession(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	sess := &Session{Messages: []Message{
		{Text: "older", Timestamp: start.Add(-time.Second)},
		{Text: "at-start", Timestamp: start},
		{Text: "at-end", Timestamp: end},
		{Text: "later", Timestamp: end.Add(time.Second)},
		{Text: "unknown-time"},
	}}
	got := buildSummaryTranscript(sess, SummaryContext{WindowStart: start, WindowEnd: end})
	for _, want := range []string{"at-start", "at-end", "unknown-time"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "older") || strings.Contains(got, "later") {
		t.Fatalf("out-of-window text: %s", got)
	}
	if len(sess.Messages) != 5 || sess.Messages[0].Text != "older" {
		t.Fatal("summary filtering mutated matching input")
	}
}

func TestAgentSummarizer_NoRelevantTranscript(t *testing.T) {
	now := time.Now()
	fa := &fakeAgent{output: `{"summary":"unrelated"}`}
	_, err := NewAgentSummarizer(fa, "").Summarize(context.Background(), &Session{Messages: []Message{{Text: "old task", Timestamp: now.Add(-time.Hour)}}}, SummaryContext{WindowStart: now, WindowEnd: now})
	if err == nil || fa.lastPrompt != "" {
		t.Fatal("empty scoped transcript must not invent intent from the diff")
	}
}

func TestSummaryContext_SanitizesBeforeClamping(t *testing.T) {
	// A credential crossing the cap must be redacted in full before the prefix
	// is selected. The multibyte suffix must remain valid UTF-8 as well.
	text := strings.Repeat("a", maxSummaryDiffBytes-10) + fakeGitHubPAT + strings.Repeat("界", maxSummaryDiffBytes)
	got := sanitizeSummaryText(text, maxSummaryDiffBytes)
	if len(got) > maxSummaryDiffBytes || !utf8.ValidString(got) || !strings.Contains(got, "truncated") || strings.Contains(got, "ghp_") {
		t.Fatal("unsafe or unbounded diff prefix")
	}
	got = sanitizeSummaryText(strings.Repeat("界", maxSummaryDiffBytes), maxSummaryDiffBytes)
	if !utf8.ValidString(got) || len(got) > maxSummaryDiffBytes {
		t.Fatal("truncated a Unicode codepoint")
	}
}

func TestExtract_CacheIsBoundToChangeAndWindow(t *testing.T) {
	now := time.Now()
	sess := &Session{AgentName: "claude", SessionID: "long", LastMsgKey: "unchanged", LastActivity: now, Messages: []Message{{Text: "edit current.txt"}}}
	cache := NewMemCache()
	cache.Put(cacheKeyFor(sess), "obsolete transcript-only summary", "claude", "long")
	fa := &fakeAgent{output: `{"summary":"first change"}`}
	p := ExtractParams{OriginCWD: "/tmp/repo", DiffFiles: []string{"current.txt"}, BaseTime: now.Add(-time.Hour), HeadTime: now, Readers: []Reader{&staticReader{name: "claude", sessions: []*Session{sess}}}, Cache: cache, Summarizer: NewAgentSummarizer(fa, ""), Change: ChangeContext{Base: "base", Head: "head-1", Diff: "first patch"}}
	first, err := Extract(context.Background(), p)
	if err != nil || first.Summary != "first change" {
		t.Fatalf("first extraction: %+v, %v", first, err)
	}
	fa.output = `{"summary":"second change"}`
	p.Change.Head, p.Change.Diff = "head-2", "second patch"
	second, err := Extract(context.Background(), p)
	if err != nil || second.Summary != "second change" {
		t.Fatalf("second extraction reused another change: %+v, %v", second, err)
	}
	fa.output = `{"summary":"wrong if called"}`
	again, err := Extract(context.Background(), p)
	if err != nil || again.Summary != "second change" {
		t.Fatalf("identical change missed cache: %+v, %v", again, err)
	}
	fa.output = `{"summary":"new window"}`
	p.BaseTime = p.BaseTime.Add(-time.Hour)
	window, err := Extract(context.Background(), p)
	if err != nil || window.Summary != "new window" {
		t.Fatalf("changed window reused old summary: %+v, %v", window, err)
	}
}

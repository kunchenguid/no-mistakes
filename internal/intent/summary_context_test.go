package intent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAgentSummarizer_PreservesNewestOverBudgetTurn(t *testing.T) {
	for _, kind := range []string{"many-turns", "oversized-last-turn"} {
		t.Run(kind, func(t *testing.T) {
			var messages []Message
			if kind == "many-turns" {
				for i := 0; i < 1000; i++ {
					messages = append(messages, Message{Role: RoleAssistant, Text: strings.Repeat("x", 256)})
				}
			} else {
				messages = append(messages, Message{Role: RoleUser, Text: strings.Repeat("x", maxTranscriptBytes+1)})
			}
			messages = append(messages, Message{Role: RoleUser, Text: strings.Repeat("y", maxTranscriptBytes*2) + "NEWEST_REQUEST"})
			if kind == "many-turns" {
				messages[len(messages)-1].Text = "NEWEST_REQUEST"
			}
			fa := &fakeAgent{output: `{"summary":"current task"}`}
			_, err := NewAgentSummarizer(fa, "").Summarize(context.Background(), &Session{Messages: messages}, SummaryContext{})
			if err != nil {
				t.Fatal(err)
			}
			var packet struct{ Transcript string }
			_, data, ok := strings.Cut(fa.lastPrompt, "\n---\n")
			if !ok || json.Unmarshal([]byte(data), &packet) != nil {
				t.Fatal("summary request must carry a JSON packet")
			}
			if !strings.HasSuffix(packet.Transcript, "NEWEST_REQUEST") {
				t.Fatal("summary request dropped the newest user request")
			}
			if len(packet.Transcript) > 80*1024 {
				t.Fatal("summary request did not bound the transcript")
			}
		})
	}
}

func TestAgentSummarizer_PiMessageTimes(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, recordType := range []string{"message", "message_end", "turn_end", "agent_end"} {
		for _, timestampKind := range []string{"absent", "string", "milliseconds"} {
			t.Run(recordType+"/"+timestampKind, func(t *testing.T) {
				var lines []string
				for _, role := range []string{"user", "assistant"} {
					for _, phase := range []string{"current", "later"} {
						at := start.Add(time.Minute)
						if phase == "later" {
							at = end.Add(time.Minute)
						}
						message := map[string]any{"role": role, "content": phase + "-" + role}
						if timestampKind == "milliseconds" {
							message["timestamp"] = at.UnixMilli()
						} else if timestampKind == "string" || phase == "later" {
							message["timestamp"] = at.Format(time.RFC3339Nano)
						}
						outerTime := at
						if recordType == "agent_end" || timestampKind != "absent" {
							outerTime = end.Add(2 * time.Minute)
						}
						record := map[string]any{"type": recordType, "timestamp": outerTime.Format(time.RFC3339Nano)}
						if recordType == "agent_end" {
							record["messages"] = []any{message}
						} else {
							record["message"] = message
						}
						line, err := json.Marshal(record)
						if err != nil {
							t.Fatal(err)
						}
						lines = append(lines, string(line))
					}
				}
				path := filepath.Join(t.TempDir(), "synthetic.jsonl")
				if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				sess := &Session{startedAtPath: path}
				if err := NewPiReader().Load(context.Background(), sess); err != nil {
					t.Fatal(err)
				}
				if len(sess.Messages) != 4 {
					t.Fatalf("loaded %d messages, want 4", len(sess.Messages))
				}
				for _, i := range []int{0, 2} {
					want := start.Add(time.Minute)
					if recordType == "agent_end" && timestampKind == "absent" {
						want = time.Time{}
					}
					if !sess.Messages[i].Timestamp.Equal(want) {
						t.Fatalf("message timestamp = %v, want %v", sess.Messages[i].Timestamp, want)
					}
				}
				fa := &fakeAgent{output: `{"summary":"current task"}`}
				_, err := NewAgentSummarizer(fa, "").Summarize(context.Background(), sess, SummaryContext{WindowStart: start, WindowEnd: end})
				if err != nil {
					t.Fatal(err)
				}
				var packet struct{ Transcript string }
				_, data, ok := strings.Cut(fa.lastPrompt, "\n---\n")
				if !ok || json.Unmarshal([]byte(data), &packet) != nil {
					t.Fatal("summary request must carry a JSON packet")
				}
				for _, role := range []string{"user", "assistant"} {
					if !strings.Contains(packet.Transcript, "current-"+role) || strings.Contains(packet.Transcript, "later-"+role) {
						t.Fatalf("summary request chose the wrong turns: %s", packet.Transcript)
					}
				}
			})
		}
	}
}

func TestBuildSummaryTranscript_EndBoundDoesNotChangeSession(t *testing.T) {
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
	for _, want := range []string{"older", "at-start", "at-end", "unknown-time"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "later") {
		t.Fatalf("out-of-window text: %s", got)
	}
	if len(sess.Messages) != 5 || sess.Messages[0].Text != "older" {
		t.Fatal("summary filtering mutated matching input")
	}
}

func TestAgentSummarizer_CommitSecondPrecision(t *testing.T) {
	end := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	sess := &Session{Messages: []Message{
		{Role: RoleUser, Text: "same-second request", Timestamp: end.Add(200 * time.Millisecond)},
		{Role: RoleAssistant, Text: "same-second completion", Timestamp: end.Add(999 * time.Millisecond)},
		{Role: RoleUser, Text: "next-second request", Timestamp: end.Add(time.Second)},
	}}
	fa := &fakeAgent{output: `{"summary":"current task"}`}
	_, err := NewAgentSummarizer(fa, "").Summarize(context.Background(), sess, SummaryContext{WindowEnd: end})
	if err != nil {
		t.Fatal(err)
	}
	var packet struct{ Transcript string }
	_, data, ok := strings.Cut(fa.lastPrompt, "\n---\n")
	if !ok || json.Unmarshal([]byte(data), &packet) != nil {
		t.Fatal("summary request must carry a JSON packet")
	}
	for _, want := range []string{"same-second request", "same-second completion"} {
		if !strings.Contains(packet.Transcript, want) {
			t.Errorf("summary request dropped %q: %s", want, packet.Transcript)
		}
	}
	if strings.Contains(packet.Transcript, "next-second request") {
		t.Fatal("summary request included a later task")
	}
	if len(sess.Messages) != 3 || !sess.Messages[0].Timestamp.Equal(end.Add(200*time.Millisecond)) {
		t.Fatal("summary filtering mutated the session")
	}
}

func TestAgentSummarizer_FutureOnlyTranscript(t *testing.T) {
	now := time.Now()
	fa := &fakeAgent{output: `{"summary":"unrelated"}`}
	_, err := NewAgentSummarizer(fa, "").Summarize(context.Background(), &Session{Messages: []Message{{Text: "future task", Timestamp: now.Add(time.Hour)}}}, SummaryContext{WindowStart: now, WindowEnd: now})
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
	sess := &Session{AgentName: "claude", SessionID: "long", LastMsgKey: "unchanged", LastActivity: now, Messages: []Message{
		{Text: "earlier requirement", Timestamp: now.Add(-90 * time.Minute)},
		{Text: "edit current.txt", Timestamp: now.Add(-30 * time.Minute)},
	}}
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
	fa.output = `{"summary":"wrong start-dependent call"}`
	p.BaseTime = p.BaseTime.Add(-time.Hour)
	window, err := Extract(context.Background(), p)
	if err != nil || window.Summary != "second change" {
		t.Fatalf("discovery start changed summary reuse: %+v, %v", window, err)
	}
	fa.output = `{"summary":"new end bound"}`
	p.HeadTime = p.HeadTime.Add(-time.Hour)
	window, err = Extract(context.Background(), p)
	if err != nil || window.Summary != "new end bound" {
		t.Fatalf("changed scoped transcript reused old summary: %+v, %v", window, err)
	}
}

func TestExtract_CacheTracksScopedContent(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		mutate    func(*ExtractParams, *Session)
		wantReuse bool
	}{
		{"fallback-start-drift", func(p *ExtractParams, _ *Session) { p.BaseTime = p.BaseTime.Add(time.Nanosecond) }, true},
		{"end-drift", func(p *ExtractParams, _ *Session) { p.HeadTime = p.HeadTime.Add(time.Nanosecond) }, true},
		{"older-transcript", func(_ *ExtractParams, s *Session) { s.Messages[1].Text = "other earlier requirement" }, false},
		{"excluded-transcript", func(_ *ExtractParams, s *Session) { s.Messages[3].Text = "other future requirement" }, true},
		{"redacted-transcript", func(_ *ExtractParams, s *Session) { s.Messages[4].Text = "use ghp_" + strings.Repeat("b", 30) }, true},
		{"earlier-discovery-start", func(p *ExtractParams, _ *Session) { p.BaseTime = p.BaseTime.Add(-time.Hour) }, true},
		{"later-discovery-start", func(p *ExtractParams, _ *Session) { p.BaseTime = now.Add(-20 * time.Minute) }, true},
		{"end-includes-turn", func(p *ExtractParams, _ *Session) { p.HeadTime = p.HeadTime.Add(2 * time.Minute) }, false},
		{"end-excludes-turn", func(p *ExtractParams, _ *Session) { p.HeadTime = now.Add(-45 * time.Minute) }, false},
		{"base", func(p *ExtractParams, _ *Session) { p.Change.Base = "other-base" }, false},
		{"head", func(p *ExtractParams, _ *Session) { p.Change.Head = "other-head" }, false},
		{"diff", func(p *ExtractParams, _ *Session) { p.Change.Diff = "other patch" }, false},
		{"session", func(_ *ExtractParams, s *Session) { s.SessionID = "other-session" }, false},
		{"transcript", func(_ *ExtractParams, s *Session) { s.Messages[0].Text = "edit current.txt differently" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &Session{AgentName: "claude", SessionID: "long", LastMsgKey: "unchanged", LastActivity: now, Messages: []Message{
				{Text: "edit current.txt"},
				{Text: "earlier requirement", Timestamp: now.Add(-7*24*time.Hour - 30*time.Minute)},
				{Text: "current requirement", Timestamp: now.Add(-30 * time.Minute)},
				{Text: "future requirement", Timestamp: now.Add(time.Minute)},
				{Text: "use " + fakeGitHubPAT},
			}}
			fa := &fakeAgent{output: `{"summary":"original summary"}`}
			p := ExtractParams{
				OriginCWD:  t.TempDir(),
				DiffFiles:  []string{"current.txt"},
				BaseTime:   now.Add(-7 * 24 * time.Hour),
				HeadTime:   now,
				Readers:    []Reader{&staticReader{name: "claude", sessions: []*Session{sess}}},
				Cache:      NewMemCache(),
				Summarizer: NewAgentSummarizer(fa, ""),
				Change:     ChangeContext{Base: "base", Head: "head", Diff: "patch"},
			}
			first, err := Extract(context.Background(), p)
			if err != nil || first.Summary != "original summary" {
				t.Fatalf("first extraction = %+v, %v", first, err)
			}
			tc.mutate(&p, sess)
			fa.lastPrompt = ""
			fa.output = `{"summary":"fresh summary"}`
			second, err := Extract(context.Background(), p)
			want := "fresh summary"
			if tc.wantReuse {
				want = "original summary"
			}
			if err != nil || second.Summary != want {
				t.Fatalf("second extraction = %+v, %v, want %q", second, err, want)
			}
			if (fa.lastPrompt == "") != tc.wantReuse {
				t.Fatalf("summary agent invocation did not match cache reuse = %v", tc.wantReuse)
			}
		})
	}
}

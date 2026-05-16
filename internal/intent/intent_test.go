package intent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// staticReader is a minimal Reader for tests.
type staticReader struct {
	name     string
	sessions []*Session
	opts     DiscoverOpts
}

func (s *staticReader) Name() string { return s.name }
func (s *staticReader) Discover(_ context.Context, opts DiscoverOpts) ([]*Session, error) {
	s.opts = opts
	return s.sessions, nil
}
func (s *staticReader) Load(_ context.Context, _ *Session) error { return nil }

type fixedSummarizer struct {
	summary string
	calls   int
}

func (f *fixedSummarizer) Summarize(_ context.Context, _ *Session) (string, error) {
	f.calls++
	return f.summary, nil
}

func TestExtract_HappyPath(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: time.Now(),
			LastMsgKey:   "k1",
			Messages: []Message{
				{Role: RoleUser, Text: "edit foo.go"},
				{Role: RoleAssistant, Text: "done", FilePaths: []string{"foo.go"}},
			},
		}},
	}
	sum := &fixedSummarizer{summary: "user edited foo"}
	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		SlackDays:  3,
		Threshold:  0.2,
		Readers:    []Reader{r},
		Cache:      NewMemCache(),
		Summarizer: sum,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Summary != "user edited foo" {
		t.Errorf("summary = %q", got.Summary)
	}
	if got.AgentName != "claude" {
		t.Errorf("agent = %q", got.AgentName)
	}
}

func TestExtract_NoMatchBelowThreshold(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: time.Now(),
			Messages:     []Message{{Role: RoleUser, Text: "hello"}},
		}},
	}
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.5,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "x"},
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("expected ErrNoMatch, got %v", err)
	}
}

func TestExtract_PassesUnextendedHeadTimeToReaders(t *testing.T) {
	baseTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	headTime := baseTime.Add(2 * time.Hour)
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: headTime,
			Messages:     []Message{{Role: RoleUser, Text: "edit foo.go", FilePaths: []string{"foo.go"}}},
		}},
	}

	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		BaseTime:   baseTime,
		HeadTime:   headTime,
		SlackDays:  3,
		Threshold:  0.1,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "edited foo"},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !r.opts.WindowEnd.Equal(headTime) {
		t.Fatalf("WindowEnd = %v, want %v", r.opts.WindowEnd, headTime)
	}
}

func TestExtract_CacheHitSkipsSummarizer(t *testing.T) {
	sess := &Session{
		SessionID:    "s1",
		LastActivity: time.Now(),
		LastMsgKey:   "k1",
		Messages:     []Message{{Role: RoleUser, Text: "x", FilePaths: []string{"foo.go"}}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{sess}}
	sum := &fixedSummarizer{summary: "fresh"}
	cache := NewMemCache()
	// Pre-populate cache with the key the extractor will compute. Note we need
	// to set AgentName first because Discover does it inside Extract; mimic.
	sess.AgentName = "claude"
	cache.Put(cacheKeyFor(sess), "cached", "claude", "s1")

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.1,
		Readers:    []Reader{r},
		Cache:      cache,
		Summarizer: sum,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Summary != "cached" {
		t.Errorf("expected cache hit summary, got %q", got.Summary)
	}
	if sum.calls != 0 {
		t.Errorf("summarizer should not have been called, got %d calls", sum.calls)
	}
}

func TestExtract_NoReaders(t *testing.T) {
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		Summarizer: &fixedSummarizer{},
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("expected ErrNoMatch with no readers, got %v", err)
	}
}

func TestExtract_LogsCandidateDecisions(t *testing.T) {
	r := &staticReader{
		name: "opencode",
		sessions: []*Session{{
			SessionID:    "weak",
			CWD:          "/tmp/repo",
			LastActivity: time.Now(),
			Messages:     []Message{{FilePaths: []string{"a.go"}}},
		}},
	}
	var logs []string
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"a.go", "b.go", "c.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "x"},
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"candidate", "opencode", "weak", "score 0.33", "rejected"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing %q:\n%s", want, joined)
		}
	}
}

func TestExtract_RequiresOriginCWD(t *testing.T) {
	_, err := Extract(context.Background(), ExtractParams{
		DiffFiles:  []string{"foo.go"},
		Summarizer: &fixedSummarizer{},
	})
	if err == nil {
		t.Error("expected error when OriginCWD missing")
	}
}

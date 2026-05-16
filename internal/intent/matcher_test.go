package intent

import (
	"testing"
	"time"
)

func TestScore_BasicOverlap(t *testing.T) {
	s := &Session{
		Messages: []Message{{
			Text:      "edited internal/foo.go",
			FilePaths: []string{"internal/bar.go"},
		}},
	}
	got, overlap := score(s, []string{"internal/foo.go", "internal/bar.go", "internal/baz.go"})
	if got <= 0 || got >= 1 {
		t.Errorf("score = %v, want strictly between 0 and 1", got)
	}
	if len(overlap) != 2 {
		t.Errorf("overlap = %v, want 2 files", overlap)
	}
}

func TestScore_BasenameOnlyMention(t *testing.T) {
	s := &Session{
		Messages: []Message{{
			Text: "look at foo.go for the change",
		}},
	}
	got, overlap := score(s, []string{"internal/sub/foo.go"})
	if got != 1.0 {
		t.Errorf("score = %v, want 1.0 (basename match)", got)
	}
	if len(overlap) != 1 {
		t.Errorf("expected overlap, got %v", overlap)
	}
}

func TestScore_NoMessages(t *testing.T) {
	s := &Session{}
	got, _ := score(s, []string{"foo.go"})
	if got != 0 {
		t.Errorf("empty session should score 0, got %v", got)
	}
}

func TestPickMatch_TieBreakByRecency(t *testing.T) {
	older := &Session{
		LastActivity: time.Now().Add(-2 * time.Hour),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	newer := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	got := pickMatch([]*Session{older, newer}, []string{"foo.go"}, 0.1)
	if got == nil {
		t.Fatal("expected a match")
	}
	if got.Session != newer {
		t.Errorf("expected newer session to win the tie")
	}
}

func TestPickMatch_BelowThreshold(t *testing.T) {
	s := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	// 1 of 10 files matches → score 0.1, threshold 0.5 → no match.
	diff := []string{"foo.go", "a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go", "i.go"}
	got := pickMatch([]*Session{s}, diff, 0.5)
	if got != nil {
		t.Errorf("expected no match below threshold, got %+v", got)
	}
}

func TestPickMatch_HigherScoreWins(t *testing.T) {
	low := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	high := &Session{
		LastActivity: time.Now().Add(-time.Hour), // older - but should still win on score
		Messages:     []Message{{FilePaths: []string{"foo.go", "bar.go"}}},
	}
	got := pickMatch([]*Session{low, high}, []string{"foo.go", "bar.go"}, 0.1)
	if got == nil || got.Session != high {
		t.Errorf("expected higher-score session to win, got %+v", got)
	}
}

func TestPickMatch_RejectsSingleOverlapInMultiFileDiff(t *testing.T) {
	s := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"internal/cli/update_test.go"}}},
	}
	diff := []string{
		"internal/cli/update.go",
		"internal/cli/update_test.go",
		"internal/update/daemon.go",
		"internal/update/update.go",
		"internal/update/update_test.go",
	}

	got := pickMatch([]*Session{s}, diff, 0.2)
	if got != nil {
		t.Fatalf("expected no match for 1/5 overlap, got %+v", got)
	}
}

func TestPickMatch_AllowsSingleOverlapForSingleFileDiff(t *testing.T) {
	s := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"internal/update/update.go"}}},
	}

	got := pickMatch([]*Session{s}, []string{"internal/update/update.go"}, 0.2)
	if got == nil {
		t.Fatal("expected single-file match")
	}
}

func TestPickMatch_RejectsOldPartialMatch(t *testing.T) {
	headTime := time.Date(2026, 5, 15, 18, 53, 52, 0, time.UTC)
	old := &Session{
		LastActivity: headTime.Add(-48 * time.Hour),
		Messages: []Message{{FilePaths: []string{
			"internal/cli/update.go",
			"internal/cli/update_test.go",
			"internal/update/daemon.go",
		}}},
	}
	diff := []string{
		"internal/cli/update.go",
		"internal/cli/update_test.go",
		"internal/update/daemon.go",
		"internal/update/update.go",
		"internal/update/update_test.go",
	}

	got := pickMatchWithOptions([]*Session{old}, diff, matchOptions{Threshold: 0.2, HeadTime: headTime})
	if got != nil {
		t.Fatalf("expected old partial match to be rejected, got %+v", got)
	}
}

func TestNormalizedPathVariants(t *testing.T) {
	got := normalizedPathVariants("./internal/foo.go")
	want := map[string]bool{"internal/foo.go": true, "foo.go": true}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected variant %q", v)
		}
		delete(want, v)
	}
	if len(want) > 0 {
		t.Errorf("missing variants: %v", want)
	}
}

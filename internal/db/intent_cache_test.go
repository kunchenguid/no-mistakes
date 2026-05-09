package db

import (
	"testing"
	"time"
)

func TestUpdateRunIntent_RoundTrip(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/intent", "git@github.com:user/intent.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	got, _ := d.GetRun(run.ID)
	if got.Intent != nil || got.IntentSource != nil || got.IntentSessionID != nil || got.IntentScore != nil {
		t.Fatalf("expected nil intent fields on fresh run, got %+v", got)
	}

	if err := d.UpdateRunIntent(run.ID, RunIntent{
		Summary:   "user wanted to add foo",
		Source:    "claude",
		SessionID: "abc-123",
		Score:     0.85,
	}); err != nil {
		t.Fatalf("update intent: %v", err)
	}

	got, _ = d.GetRun(run.ID)
	if got.Intent == nil || *got.Intent != "user wanted to add foo" {
		t.Errorf("intent = %v, want %q", got.Intent, "user wanted to add foo")
	}
	if got.IntentSource == nil || *got.IntentSource != "claude" {
		t.Errorf("intent source = %v, want claude", got.IntentSource)
	}
	if got.IntentSessionID == nil || *got.IntentSessionID != "abc-123" {
		t.Errorf("intent session = %v, want abc-123", got.IntentSessionID)
	}
	if got.IntentScore == nil || *got.IntentScore != 0.85 {
		t.Errorf("intent score = %v, want 0.85", got.IntentScore)
	}
}

func TestIntentCache_PutGet(t *testing.T) {
	d := openTestDB(t)

	got, err := d.GetIntentCache("missing")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing key, got %+v", got)
	}

	if err := d.PutIntentCache(IntentCacheEntry{
		CacheKey:  "k1",
		Summary:   "do the thing",
		AgentName: "claude",
		SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err = d.GetIntentCache("k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Summary != "do the thing" || got.AgentName != "claude" {
		t.Fatalf("got %+v", got)
	}
	if got.CreatedAt == 0 {
		t.Errorf("expected created_at populated")
	}

	// Replace.
	if err := d.PutIntentCache(IntentCacheEntry{
		CacheKey:  "k1",
		Summary:   "do the new thing",
		AgentName: "claude",
		SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = d.GetIntentCache("k1")
	if got.Summary != "do the new thing" {
		t.Errorf("want replaced summary, got %q", got.Summary)
	}
}

func TestIntentCache_Cleanup(t *testing.T) {
	d := openTestDB(t)

	now := time.Now().Unix()
	d.PutIntentCache(IntentCacheEntry{CacheKey: "old", Summary: "x", AgentName: "claude", SessionID: "s", CreatedAt: now - int64((40 * 24 * time.Hour).Seconds())})
	d.PutIntentCache(IntentCacheEntry{CacheKey: "new", Summary: "x", AgentName: "claude", SessionID: "s", CreatedAt: now})

	deleted, err := d.CleanupOldIntentCache(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if got, _ := d.GetIntentCache("old"); got != nil {
		t.Errorf("expected old entry gone")
	}
	if got, _ := d.GetIntentCache("new"); got == nil {
		t.Errorf("expected new entry retained")
	}
}

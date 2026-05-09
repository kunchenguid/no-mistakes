package intent

import "testing"

func TestMemCache_GetPut(t *testing.T) {
	c := NewMemCache()
	if _, ok := c.Get("missing"); ok {
		t.Error("missing key should return ok=false")
	}
	c.Put("k", "summary", "claude", "sess-1")
	got, ok := c.Get("k")
	if !ok || got != "summary" {
		t.Errorf("got (%q, %v), want (summary, true)", got, ok)
	}
}

func TestCacheKeyFor_DistinguishesSessionGrowth(t *testing.T) {
	a := &Session{AgentName: "claude", SessionID: "s1", LastMsgKey: "k1", Messages: []Message{{}}}
	b := &Session{AgentName: "claude", SessionID: "s1", LastMsgKey: "k1", Messages: []Message{{}, {}}}
	if cacheKeyFor(a) == cacheKeyFor(b) {
		t.Error("session growth must change the cache key")
	}
}

func TestCacheKeyFor_StableForSameSession(t *testing.T) {
	a := &Session{AgentName: "claude", SessionID: "s1", LastMsgKey: "k1", Messages: []Message{{Text: "x"}}}
	b := &Session{AgentName: "claude", SessionID: "s1", LastMsgKey: "k1", Messages: []Message{{Text: "y"}}}
	if cacheKeyFor(a) != cacheKeyFor(b) {
		t.Error("identical metadata must produce the same cache key")
	}
}

package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRoundTripAndStaleness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-check.json")
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	entry := &checkCache{CheckedAt: now, LatestVersion: "v1.2.3"}

	if err := writeCache(path, entry); err != nil {
		t.Fatalf("writeCache error = %v", err)
	}

	loaded := readCache(path)
	if loaded == nil {
		t.Fatal("readCache returned nil")
	}
	if !loaded.CheckedAt.Equal(now) {
		t.Fatalf("CheckedAt = %v, want %v", loaded.CheckedAt, now)
	}
	if loaded.LatestVersion != "v1.2.3" {
		t.Fatalf("LatestVersion = %q", loaded.LatestVersion)
	}

	if !cacheStale(loaded, "v1.2.2", now.Add(23*time.Hour)) {
		t.Fatal("a legacy pending answer should be refreshed immediately")
	}
	channelAware := &checkCache{CheckedAt: now, LatestVersion: "v1.2.3", StableVersion: "v1.2.3"}
	if cacheStale(channelAware, "v1.2.2", now.Add(23*time.Hour)) {
		t.Fatal("a pending update should stay fresh before ttl")
	}
	if !cacheStale(channelAware, "v1.2.2", now.Add(25*time.Hour)) {
		t.Fatal("a pending update should be stale after ttl")
	}
	if !cacheStale(loaded, "v1.2.2", now.Add(25*time.Hour)) {
		t.Fatal("cache should be stale after ttl")
	}
	if cacheStale(loaded, "v1.2.3", now.Add(time.Hour)) {
		t.Fatal("cache should stay fresh when current version catches up")
	}
	if cacheStale(loaded, "v1.3.0-beta.1", now.Add(time.Hour)) {
		t.Fatal("cache should stay fresh when a prerelease leads stable")
	}
	if !cacheStale(nil, "v1.2.2", now) {
		t.Fatal("nil cache should be stale")
	}

	badPath := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(badPath, []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readCache(badPath); got != nil {
		t.Fatal("corrupt cache should return nil")
	}
}

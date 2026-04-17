package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type checkCache struct {
	CheckedAt     time.Time `json:"checked_at"`
	LatestVersion string    `json:"latest_version"`
}

func writeCache(path string, entry *checkCache) error {
	if entry == nil {
		return fmt.Errorf("write cache: nil entry")
	}
	if path == "" {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write cache dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write cache file: %w", err)
	}
	return nil
}

func readCache(path string) *checkCache {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entry checkCache
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	if entry.LatestVersion == "" {
		return nil
	}
	return &entry
}

func cacheStale(entry *checkCache, currentVersion string, now time.Time) bool {
	if entry == nil {
		return true
	}
	if now.Sub(entry.CheckedAt) > cacheTTL {
		return true
	}
	cmp, err := compareVersions(currentVersion, entry.LatestVersion)
	if err != nil {
		return true
	}
	return cmp >= 0
}

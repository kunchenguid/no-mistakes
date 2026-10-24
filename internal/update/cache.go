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
	StableVersion string    `json:"stable_version,omitempty"`
}

func (u *updater) channelCachePath(includePrereleases bool) string {
	if !includePrereleases || u.cachePath == "" {
		return u.cachePath
	}
	ext := filepath.Ext(u.cachePath)
	return u.cachePath[:len(u.cachePath)-len(ext)] + "-beta" + ext
}

// GitHub prereleases can have plain semver tags. A build ahead of the stable
// channel belongs to beta even without a semver prerelease suffix.
func runningBeta(currentVersion, stableVersion string) bool {
	version, err := parseVersion(currentVersion)
	if err == nil && len(version.prerelease) > 0 {
		return true
	}
	cmp, err := compareVersions(currentVersion, stableVersion)
	return err == nil && cmp > 0
}

func (u *updater) notificationCache() *checkCache {
	stable := readCache(u.channelCachePath(false))
	beta := readCache(u.channelCachePath(true))
	stableVersion := ""
	if stable != nil {
		stableVersion = stable.LatestVersion
	}
	if beta != nil && beta.StableVersion != "" && (stable == nil || beta.CheckedAt.After(stable.CheckedAt)) {
		stableVersion = beta.StableVersion
	}
	if runningBeta(u.currentVersion, stableVersion) && beta != nil {
		return beta
	}
	// Honour a recent legacy stable answer until its TTL expires, including
	// when this build is ahead of it and has no beta cache yet.
	return stable
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
	return cmp < 0
}

package db

import (
	"database/sql"
	"fmt"
	"time"
)

// IntentCacheEntry is a cached summarization for a known agent session.
type IntentCacheEntry struct {
	CacheKey  string
	Summary   string
	AgentName string
	SessionID string
	CreatedAt int64
}

// GetIntentCache returns the cached summary for a key, or nil if absent.
func (d *DB) GetIntentCache(key string) (*IntentCacheEntry, error) {
	e := &IntentCacheEntry{}
	err := d.sql.QueryRow(
		`SELECT cache_key, summary, agent_name, session_id, created_at FROM intent_cache WHERE cache_key = ?`, key,
	).Scan(&e.CacheKey, &e.Summary, &e.AgentName, &e.SessionID, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get intent cache: %w", err)
	}
	return e, nil
}

// PutIntentCache inserts or replaces an intent cache entry.
func (d *DB) PutIntentCache(e IntentCacheEntry) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = now()
	}
	_, err := d.sql.Exec(
		`INSERT OR REPLACE INTO intent_cache (cache_key, summary, agent_name, session_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		e.CacheKey, e.Summary, e.AgentName, e.SessionID, e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("put intent cache: %w", err)
	}
	return nil
}

// CleanupOldIntentCache deletes entries older than maxAge. Returns rows deleted.
func (d *DB) CleanupOldIntentCache(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).Unix()
	result, err := d.sql.Exec(`DELETE FROM intent_cache WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("cleanup intent cache: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

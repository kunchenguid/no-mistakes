package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// Cache abstracts the summarization cache behind a small interface so the
// extractor can be exercised without a real DB in tests.
type Cache interface {
	Get(key string) (string, bool)
	Put(key, summary, agentName, sessionID string)
}

// dbCache is the production cache backed by db.IntentCache* methods.
type dbCache struct {
	db *db.DB
}

// NewDBCache wraps a *db.DB as a Cache.
func NewDBCache(database *db.DB) Cache {
	if database == nil {
		return memCache{}
	}
	return &dbCache{db: database}
}

func (c *dbCache) Get(key string) (string, bool) {
	entry, err := c.db.GetIntentCache(key)
	if err != nil || entry == nil {
		return "", false
	}
	return entry.Summary, true
}

func (c *dbCache) Put(key, summary, agentName, sessionID string) {
	_ = c.db.PutIntentCache(db.IntentCacheEntry{
		CacheKey:  key,
		Summary:   summary,
		AgentName: agentName,
		SessionID: sessionID,
	})
}

// memCache is an in-memory fallback used when the DB is unavailable and in tests.
type memCache map[string]string

// NewMemCache returns an in-memory Cache. Mainly for tests.
func NewMemCache() Cache { return memCache{} }

func (c memCache) Get(key string) (string, bool) { v, ok := c[key]; return v, ok }
func (c memCache) Put(key, summary, _, _ string) { c[key] = summary }

// cacheKeyFor derives a deterministic cache key. We include the agent name,
// session id, last-message key, and message count - the latter two are
// independent stale-detection signals so a buggy reader that fails to
// update LastMsgKey on append still gets cache misses on growth.
func cacheKeyFor(s *Session) string {
	h := sha256.New()
	h.Write([]byte(s.AgentName))
	h.Write([]byte{'|'})
	h.Write([]byte(s.SessionID))
	h.Write([]byte{'|'})
	h.Write([]byte(s.LastMsgKey))
	h.Write([]byte{'|'})
	h.Write([]byte(strconv.Itoa(len(s.Messages))))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

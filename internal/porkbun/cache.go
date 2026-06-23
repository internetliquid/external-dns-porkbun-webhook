package porkbun

import (
	"sync"
	"time"
)

// zoneCache caches RetrieveRecords results per zone for a bounded TTL so that
// repeated reconciles don't re-list the same zone. A non-positive TTL disables
// caching entirely.
type zoneCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	records []Record
	expires time.Time
}

func newZoneCache(ttl time.Duration, now func() time.Time) *zoneCache {
	return &zoneCache{
		ttl:     ttl,
		now:     now,
		entries: make(map[string]cacheEntry),
	}
}

// get returns the cached records for a zone and true if a non-expired entry
// exists. The returned slice is a copy, so callers can mutate it freely without
// corrupting the cache.
func (c *zoneCache) get(zone string) ([]Record, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[zone]
	if !ok || c.now().After(e.expires) {
		return nil, false
	}
	return cloneRecords(e.records), true
}

// set stores a copy of records for a zone with a fresh TTL.
func (c *zoneCache) set(zone string, records []Record) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[zone] = cacheEntry{
		records: cloneRecords(records),
		expires: c.now().Add(c.ttl),
	}
}

// invalidate drops any cached entry for a zone. Mutating operations call this
// so the next read reflects the change instead of stale cached state.
func (c *zoneCache) invalidate(zone string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, zone)
}

func cloneRecords(in []Record) []Record {
	if in == nil {
		return nil
	}
	out := make([]Record, len(in))
	copy(out, in)
	return out
}

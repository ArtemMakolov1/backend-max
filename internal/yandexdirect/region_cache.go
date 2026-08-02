package yandexdirect

import (
	"strings"
	"sync"
	"time"
)

const directRegionCacheTTL = 6 * time.Hour

type directRegionCacheEntry struct {
	regions   []GeoRegion
	expiresAt time.Time
}

type directRegionCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]directRegionCacheEntry
}

func newDirectRegionCache() *directRegionCache {
	return &directRegionCache{
		now: time.Now, entries: make(map[string]directRegionCacheEntry),
	}
}

func (c *directRegionCache) get(names []string) ([]GeoRegion, bool) {
	if c == nil {
		return nil, false
	}
	key := directRegionCacheKey(names)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !entry.expiresAt.After(c.now().UTC()) {
		delete(c.entries, key)
		return nil, false
	}
	return cloneGeoRegions(entry.regions), true
}

func (c *directRegionCache) put(names []string, regions []GeoRegion) {
	if c == nil || len(names) == 0 || len(regions) != len(names) {
		return
	}
	c.mu.Lock()
	c.entries[directRegionCacheKey(names)] = directRegionCacheEntry{
		regions: cloneGeoRegions(regions), expiresAt: c.now().UTC().Add(directRegionCacheTTL),
	}
	c.mu.Unlock()
}

func directRegionCacheKey(names []string) string {
	keys := make([]string, 0, len(names))
	for _, name := range names {
		keys = append(keys, graphFold(name))
	}
	return strings.Join(keys, "\x00")
}

func cloneGeoRegions(values []GeoRegion) []GeoRegion {
	cloned := make([]GeoRegion, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].ParentNames = append([]string(nil), value.ParentNames...)
	}
	return cloned
}

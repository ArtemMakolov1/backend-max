package yandexdirect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	directCurrencyCacheTTL        = 12 * time.Hour
	directCurrencyCacheMaxEntries = 512
)

type directCurrencyCacheEntry struct {
	values    map[string]CurrencyBidLimits
	expiresAt time.Time
}

// directCurrencyCache is intentionally owned by one Client instance, so its
// API base URL and sandbox/live environment are already part of the cache
// boundary. The remaining key identifies the advertiser authorization scope
// without retaining an access token in plaintext.
type directCurrencyCache struct {
	mu      sync.Mutex
	entries map[string]directCurrencyCacheEntry
	group   singleflight.Group
	now     func() time.Time
	ttl     time.Duration
}

func newDirectCurrencyCache() *directCurrencyCache {
	return &directCurrencyCache{
		entries: make(map[string]directCurrencyCacheEntry),
		now:     time.Now,
		ttl:     directCurrencyCacheTTL,
	}
}

func (c *directCurrencyCache) get(
	ctx context.Context, key string,
	load func(context.Context) (map[string]CurrencyBidLimits, error),
) (map[string]CurrencyBidLimits, error) {
	if values, ok := c.cached(key); ok {
		return values, nil
	}
	result := c.group.DoChan(key, func() (any, error) {
		if values, ok := c.cached(key); ok {
			return values, nil
		}
		values, err := load(ctx)
		if err != nil {
			return nil, err
		}
		now := c.now().UTC()
		c.store(key, values, now)
		return values, nil
	})
	select {
	case outcome := <-result:
		if outcome.Err != nil {
			return nil, outcome.Err
		}
		return outcome.Val.(map[string]CurrencyBidLimits), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *directCurrencyCache) store(
	key string, values map[string]CurrencyBidLimits, now time.Time,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for cachedKey, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, cachedKey)
		}
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= directCurrencyCacheMaxEntries {
		evictKey := ""
		var earliestExpiry time.Time
		for cachedKey, entry := range c.entries {
			if evictKey == "" || entry.expiresAt.Before(earliestExpiry) {
				evictKey, earliestExpiry = cachedKey, entry.expiresAt
			}
		}
		delete(c.entries, evictKey)
	}
	c.entries[key] = directCurrencyCacheEntry{
		values: values, expiresAt: now.Add(c.ttl),
	}
}

func (c *directCurrencyCache) cached(key string) (map[string]CurrencyBidLimits, bool) {
	if c == nil {
		return nil, false
	}
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.After(now) {
		delete(c.entries, key)
		return nil, false
	}
	return entry.values, true
}

func directCurrencyCacheKey(token, clientLogin string) string {
	if login := strings.ToLower(strings.TrimSpace(clientLogin)); login != "" {
		return "login:" + login
	}
	// A missing Client-Login means the access token determines the advertiser.
	// Hash it so cache keys cannot expose bearer credentials in a dump or log.
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return "token-sha256:" + hex.EncodeToString(sum[:])
}

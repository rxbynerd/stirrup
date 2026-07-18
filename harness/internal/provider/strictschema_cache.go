package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
)

// strictSchemaCache memoises NormalizeStrictSchema results within a single
// adapter instance (one per run, so a cache entry cannot leak across runs).
// The cache key is (model, tool-name, schema-bytes-hash); design rationale
// in docs/provider-quirks.md.
//
// Hits / Misses are exposed to tests but are not part of the public
// adapter contract.
type strictSchemaCache struct {
	mu      sync.RWMutex
	entries map[strictSchemaCacheKey]json.RawMessage

	Hits   atomic.Uint64
	Misses atomic.Uint64
}

// strictSchemaCacheKey identifies a normalised schema by (model,
// tool-name, schema-hash).
type strictSchemaCacheKey struct {
	model    string
	toolName string
	hash     string
}

// newStrictSchemaCache returns an initialised cache. Pass it by pointer —
// copying would clone the mutex and counters.
func newStrictSchemaCache() *strictSchemaCache {
	return &strictSchemaCache{
		entries: map[strictSchemaCacheKey]json.RawMessage{},
	}
}

// lookup returns the cached normalised schema, or nil if absent.
func (c *strictSchemaCache) lookup(key strictSchemaCacheKey) (json.RawMessage, bool) {
	c.mu.RLock()
	out, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		c.Hits.Add(1)
	}
	return out, ok
}

// schemaHash returns a hex-encoded SHA-256 of the input bytes, used by
// the cache key.
func schemaHash(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// normalizeStrictWithCache is the call shape adapters use: it consults
// the cache, runs NormalizeStrictSchema on a miss, stores the result,
// and returns the cached bytes. Errors are NOT cached — a schema that
// fails the strict-mode lint re-fails on every turn rather than papering
// over a transient parse problem.
func normalizeStrictWithCache(cache *strictSchemaCache, model, toolName string, raw json.RawMessage) (json.RawMessage, error) {
	if cache == nil {
		return NormalizeStrictSchema(toolName, raw)
	}
	key := strictSchemaCacheKey{
		model:    model,
		toolName: toolName,
		hash:     schemaHash(raw),
	}
	if cached, ok := cache.lookup(key); ok {
		return cached, nil
	}
	return cache.computeAndStore(key, toolName, raw)
}

// computeAndStore handles the miss path under a write lock, giving the
// cache singleflight semantics: holding the lock for the duration of
// NormalizeStrictSchema means only one goroutine runs the normaliser per
// key, and a concurrent loser observes the winner's entry on its
// re-check. Misses increments only on a genuine, successful miss.
func (c *strictSchemaCache) computeAndStore(key strictSchemaCacheKey, toolName string, raw json.RawMessage) (json.RawMessage, error) {
	c.mu.Lock()
	if cached, ok := c.entries[key]; ok {
		c.mu.Unlock()
		c.Hits.Add(1)
		return cached, nil
	}
	out, err := NormalizeStrictSchema(toolName, raw)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.entries[key] = out
	c.Misses.Add(1)
	c.mu.Unlock()
	return out, nil
}

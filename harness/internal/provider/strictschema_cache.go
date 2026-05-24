package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
)

// strictSchemaCache memoises NormalizeStrictSchema results within a
// single adapter instance. The factory builds one adapter per run, so
// per-adapter scope matches the "per-run" caching the design calls
// for: a tool's schema is stable within a run, and the normalisation
// is the same expensive recursive walk for every turn that re-sends
// it. Different runs route through different adapter instances, so a
// cache entry from one run cannot leak into another.
//
// The cache key is (model, tool-name, schema-bytes-hash). Including
// `model` in the key is load-bearing: a dynamic-router run can switch
// models turn to turn, and a strict-mode rule may pin different
// models to different strict-mode contracts (e.g. a future model with
// stricter `additionalProperties` semantics). Hashing the raw schema
// bytes is what protects against a runtime overwrite of the tool's
// canonical schema — if the bytes change, the hash changes, and the
// stale rewrite is bypassed.
//
// The atomic Hits / Misses counters expose the cache's effectiveness
// to tests without bolting a separate observer onto the type. They
// are intentionally not part of the public adapter contract.
type strictSchemaCache struct {
	mu      sync.RWMutex
	entries map[strictSchemaCacheKey]json.RawMessage

	Hits   atomic.Uint64
	Misses atomic.Uint64
}

// strictSchemaCacheKey identifies a normalised schema by (model,
// tool-name, schema-hash). Schema-hash is a hex-encoded SHA-256 of
// the input bytes — collision probability is vanishingly small at
// this key population and the cost is one hash per cache lookup.
type strictSchemaCacheKey struct {
	model    string
	toolName string
	hash     string
}

// newStrictSchemaCache returns an initialised cache. Callers should
// pass it by pointer; copy semantics would clone the mutex and the
// counters into a state that no longer guards anything.
func newStrictSchemaCache() *strictSchemaCache {
	return &strictSchemaCache{
		entries: map[strictSchemaCacheKey]json.RawMessage{},
	}
}

// lookup returns the cached normalised schema, or nil if absent. The
// returned RawMessage is safe to share: NormalizeStrictSchema produces
// fresh bytes each time, and the cache stores those bytes verbatim.
func (c *strictSchemaCache) lookup(key strictSchemaCacheKey) (json.RawMessage, bool) {
	c.mu.RLock()
	out, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		c.Hits.Add(1)
	}
	return out, ok
}

// store records the normalised schema. Concurrent stores of the same
// key are idempotent — the last write wins, both writes produce the
// same bytes for the same input, so there is no observable race.
func (c *strictSchemaCache) store(key strictSchemaCacheKey, value json.RawMessage) {
	c.mu.Lock()
	c.entries[key] = value
	c.mu.Unlock()
	c.Misses.Add(1)
}

// schemaHash returns a hex-encoded SHA-256 of the input bytes. Used by
// the cache key. Constant in size (64 chars) so the map's key
// equality cost is bounded regardless of schema size.
func schemaHash(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// normalizeStrictWithCache is the call shape adapters use: it consults
// the cache, runs NormalizeStrictSchema on a miss, stores the result,
// and returns the cached bytes. Errors are NOT cached — a schema that
// fails the strict-mode lint should re-fail on every turn so an
// operator can see the failure surface in logs each time, and so a
// rule change that introduces strict mode mid-run does not paper over
// a transient parse problem.
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
	out, err := NormalizeStrictSchema(toolName, raw)
	if err != nil {
		return nil, err
	}
	cache.store(key, out)
	return out, nil
}

package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
)

// strictSchemaCache memoises NormalizeStrictSchema outcomes within a single
// adapter instance (one per run, so a cache entry cannot leak across runs).
// An outcome is either a normalised schema or a lint rejection that sends
// the tool non-strict. The cache key is (model, tool-name,
// schema-bytes-hash); design rationale in docs/provider-quirks.md.
//
// Hits / Misses are exposed to tests but are not part of the public
// adapter contract.
type strictSchemaCache struct {
	mu      sync.RWMutex
	entries map[strictSchemaCacheKey]strictSchemaEntry

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

// strictSchemaEntry is one cached outcome: schema holds the normalised
// bytes and nullable the properties whose synthetic nulls are dropped (see
// optionalNonNullable), or fallback holds the lint rejection that sends the
// tool non-strict. Cached entries are shared across requests, so nullable
// is read-only.
type strictSchemaEntry struct {
	schema   json.RawMessage
	nullable map[string]struct{}
	fallback *strictSchemaError
}

// newStrictSchemaCache returns an initialised cache. Pass it by pointer —
// copying would clone the mutex and counters.
func newStrictSchemaCache() *strictSchemaCache {
	return &strictSchemaCache{
		entries: map[strictSchemaCacheKey]strictSchemaEntry{},
	}
}

// lookup returns the cached outcome for key.
func (c *strictSchemaCache) lookup(key strictSchemaCacheKey) (strictSchemaEntry, bool) {
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

// strictToolSchema resolves the schema a tool is sent with under strict
// mode. A schema the normaliser accepts returns the rewritten bytes and
// strict=true. A schema it rejects falls back to the original bytes and
// strict=false, so one non-normalisable tool does not fail the request;
// the harness still validates every tool input against the original
// schema before dispatch. newFallback is non-nil only the first time the
// cache records that rejection, so callers log one WARN per cache key.
// nullable is set only for a strict tool and must not be modified.
// A malformed schema (invalid JSON, or keywords of the wrong shape) returns
// an error: it cannot validate tool input, so no request body carries it.
func strictToolSchema(cache *strictSchemaCache, model, toolName string, raw json.RawMessage) (schema json.RawMessage, nullable map[string]struct{}, strict bool, newFallback *strictSchemaError, err error) {
	entry, first, err := normalizeStrictWithCache(cache, model, toolName, raw)
	if err != nil {
		return nil, nil, false, nil, err
	}
	if entry.fallback == nil {
		return entry.schema, entry.nullable, true, nil, nil
	}
	if first {
		newFallback = entry.fallback
	}
	return raw, nil, false, newFallback, nil
}

// normalizeStrictWithCache consults the cache, runs NormalizeStrictSchema
// on a miss, stores the outcome, and reports whether this call computed
// it. A lint rejection is stored as a fallback entry so it is reported
// once per key. A malformed schema is NOT cached and returns an error, so
// a transient parse problem is not papered over.
func normalizeStrictWithCache(cache *strictSchemaCache, model, toolName string, raw json.RawMessage) (entry strictSchemaEntry, first bool, err error) {
	if cache == nil {
		return computeStrictEntry(toolName, raw)
	}
	key := strictSchemaCacheKey{
		model:    model,
		toolName: toolName,
		hash:     schemaHash(raw),
	}
	if cached, ok := cache.lookup(key); ok {
		return cached, false, nil
	}
	return cache.computeAndStore(key, toolName, raw)
}

// computeStrictEntry runs the normaliser and classifies the outcome. Only
// a lint rejection of a well-formed schema becomes a fallback entry.
func computeStrictEntry(toolName string, raw json.RawMessage) (strictSchemaEntry, bool, error) {
	out, err := NormalizeStrictSchema(toolName, raw)
	if err == nil {
		return strictSchemaEntry{schema: out, nullable: optionalNonNullable(raw)}, true, nil
	}
	var lintErr *strictSchemaError
	if errors.As(err, &lintErr) && !lintErr.malformed {
		return strictSchemaEntry{fallback: lintErr}, true, nil
	}
	return strictSchemaEntry{}, false, err
}

// computeAndStore handles the miss path under a write lock, giving the
// cache singleflight semantics: holding the lock for the duration of
// NormalizeStrictSchema means only one goroutine runs the normaliser per
// key, and a concurrent loser observes the winner's entry on its
// re-check. Misses increments only on a genuine miss that produced an
// outcome.
func (c *strictSchemaCache) computeAndStore(key strictSchemaCacheKey, toolName string, raw json.RawMessage) (strictSchemaEntry, bool, error) {
	c.mu.Lock()
	if cached, ok := c.entries[key]; ok {
		c.mu.Unlock()
		c.Hits.Add(1)
		return cached, false, nil
	}
	entry, first, err := computeStrictEntry(toolName, raw)
	if err != nil {
		c.mu.Unlock()
		return strictSchemaEntry{}, false, err
	}
	c.entries[key] = entry
	c.Misses.Add(1)
	c.mu.Unlock()
	return entry, first, nil
}

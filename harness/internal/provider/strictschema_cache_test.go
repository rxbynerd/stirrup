package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSchemaHash pins the basic correctness properties of schemaHash:
// equal inputs produce equal hashes (deterministic) and a small set of
// distinct inputs produce distinct hashes (no constant-output stub).
// The cache's correctness rests on these two properties — a future
// edit that swapped sha256 for a non-deterministic hash, or that
// returned a constant string for performance, would silently fold
// every schema into a single cache entry and serve stale rewrites
// across unrelated tools.
//
// The test is not a cryptographic collision-resistance check; that
// belongs to the underlying primitive. The point is to keep the
// interface honest against accidental replacement.
func TestSchemaHash_DeterministicDistinctLength(t *testing.T) {
	schemas := []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "object", schema: json.RawMessage(`{"type":"object"}`)},
		{name: "array", schema: json.RawMessage(`{"type":"array"}`)},
		{name: "string", schema: json.RawMessage(`{"type":"string"}`)},
		{name: "empty", schema: json.RawMessage(`{}`)},
		{name: "object with property", schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)},
	}

	t.Run("deterministic", func(t *testing.T) {
		for _, tc := range schemas {
			t.Run(tc.name, func(t *testing.T) {
				must := require.New(t)
				first := schemaHash(tc.schema)
				second := schemaHash(tc.schema)
				must.Equal(first, second)
			})
		}
	})

	t.Run("distinct inputs", func(t *testing.T) {
		must := require.New(t)
		seen := make(map[string]string, len(schemas))
		for _, tc := range schemas {
			h := schemaHash(tc.schema)
			prev, dup := seen[h]
			must.False(dup, "%s and %s hash to the same value %q", prev, tc.name, h)
			seen[h] = tc.name
		}
	})

	// A shorter digest (for example CRC32) would weaken collision resistance
	// and would not pad to 64 characters, so the cache-key comparison cost
	// stays bounded only while the length is pinned.
	t.Run("sha256 hex length", func(t *testing.T) {
		must := require.New(t)
		h := schemaHash(schemas[0].schema)
		must.Len(h, 64, "want 64 hex characters (hex-encoded SHA-256)")
	})
}

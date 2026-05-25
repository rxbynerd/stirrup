package provider

import (
	"encoding/json"
	"testing"
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
func TestSchemaHash(t *testing.T) {
	inputs := []json.RawMessage{
		json.RawMessage(`{"type":"object"}`),
		json.RawMessage(`{"type":"array"}`),
		json.RawMessage(`{"type":"string"}`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
	}

	// Determinism: hashing the same bytes twice yields the same string.
	for _, in := range inputs {
		first := schemaHash(in)
		second := schemaHash(in)
		if first != second {
			t.Errorf("schemaHash(%s) not deterministic: %q != %q", in, first, second)
		}
	}

	// Distinctness: each input in the set hashes to a unique value.
	seen := make(map[string]json.RawMessage, len(inputs))
	for _, in := range inputs {
		h := schemaHash(in)
		if prev, ok := seen[h]; ok {
			t.Errorf("schemaHash collision between %s and %s -> %q", prev, in, h)
		}
		seen[h] = in
	}

	// SHA-256 hex output is 64 chars. A drop to a shorter digest
	// (e.g. CRC32) would be a regression both because it weakens
	// collision resistance and because it would not pad to 64; pin
	// the length so the cache-key equality cost stays bounded.
	if h := schemaHash(inputs[0]); len(h) != 64 {
		t.Errorf("schemaHash length = %d, want 64 (hex-encoded SHA-256)", len(h))
	}
}

// Package toolname provides per-provider normalization and collision
// detection for tool/function names that flow from the registry into a
// provider request.
//
// Provider tool-name constraints are stricter than what Stirrup's
// registration boundaries enforce. MCP-derived names in particular can
// contain hyphens, dots, spaces, or non-ASCII codepoints that one
// provider accepts and another rejects, so a registered tool can become
// an un-callable function name on the wire. This package centralises
// those constraints into a single Policy table and produces a
// deterministic, round-trip-safe Mapping between the internal name a
// handler is registered under and the external name a provider sees.
//
// The normalization layer is intentionally provider-agnostic: callers
// pass a provider-type discriminator and receive a Mapping, never the
// concrete Policy. Adapters do not import this package — the
// normalization wrapper in harness/internal/provider sits between the
// loop and the concrete adapter and is the only consumer.
//
// TODO(#221): when the capability-profile work lands, fold PolicyFor
// into the profile so each provider declares its own naming rules
// alongside its other capabilities.
package toolname

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Policy describes the per-provider function-name constraints applied
// during normalization. The defaults pick the strictest commonly-
// supported character set so a name produced under one policy is
// likely to be acceptable under another.
//
// MaxLen bounds the externalised name after character substitution. A
// name that exceeds MaxLen is truncated and disambiguated with a short
// stable hash suffix derived from the internal name so two long names
// that share a common prefix do not collide silently.
//
// AllowHyphen / AllowLeadingDigit toggle the two real differences
// across the providers Stirrup targets today. Gemini disallows hyphens
// in function names entirely and requires the first character to be a
// letter or underscore; OpenAI and Anthropic accept both.
type Policy struct {
	MaxLen            int
	AllowHyphen       bool
	AllowLeadingDigit bool
}

// PolicyFor returns the Policy that applies to the given provider type
// string (the discriminator used in RunConfig.Provider.Type and in the
// providers map). Unknown providers fall through to the strictest
// policy so an adapter added without updating this table degrades to
// "still serialises a name the provider will accept" rather than
// "crashes the first turn".
//
// TODO(#221): move the per-provider data into the capability profile so
// each adapter declares its rules in one place.
func PolicyFor(providerType string) Policy {
	switch providerType {
	case "anthropic":
		// Anthropic accepts [a-zA-Z0-9_-]{1,64} on the Messages API
		// tool name. Leading digits are permitted; hyphens are
		// permitted.
		return Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	case "openai-compatible", "openai-responses":
		// Chat Completions and the Responses API both enforce
		// ^[a-zA-Z0-9_-]{1,64}$ on function names. Leading digits are
		// permitted in practice; hyphens are permitted.
		return Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	case "bedrock":
		// Bedrock Converse API mirrors the Anthropic constraint for
		// the Anthropic-backed models that dominate today's deployment
		// and matches the OpenAI rule for the Mistral/Llama-backed
		// models. [a-zA-Z0-9_-]{1,64} is the safe union.
		return Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	case "gemini":
		// Vertex AI function declarations require the name to match
		// `[a-zA-Z_][a-zA-Z0-9_]*` with a 64-character cap. Hyphens
		// are rejected and a leading digit is rejected.
		return Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: false}
	default:
		// Unknown provider: pick the strictest constraint set so a new
		// adapter cannot regress on naming until its policy is
		// registered here.
		return Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: false}
	}
}

// Mapping records the bidirectional translation between internal
// (registry-side) names and external (provider-facing) names for a
// single request. Both directions are pre-built so dispatch does not
// have to recompute the normalization for every inbound tool_call
// event.
//
// A name absent from ExternalFor is one the caller never passed in;
// callers should fall back to the original string in that case rather
// than treating absence as an error, so untouched fields (e.g. a
// tool_use block on a message from a prior turn whose tool has since
// been unregistered) round-trip unchanged.
type Mapping struct {
	// ExternalFor maps internal name → external name.
	ExternalFor map[string]string
	// InternalFor maps external name → internal name. This is the
	// inverse of ExternalFor and is what the adapter wrapper consults
	// when translating an inbound tool_call event back to the name
	// the registry knows the tool by.
	InternalFor map[string]string
}

// Translate returns the external name for an internal one, falling
// back to the input when the name is not in the mapping. The fallback
// keeps adapter wrappers terse — a missing entry is exactly the case
// where no normalization is required.
func (m *Mapping) Translate(internal string) string {
	if m == nil {
		return internal
	}
	if ext, ok := m.ExternalFor[internal]; ok {
		return ext
	}
	return internal
}

// Reverse returns the internal name for an external one, falling back
// to the input on a miss. See Translate for the rationale on
// pass-through.
func (m *Mapping) Reverse(external string) string {
	if m == nil {
		return external
	}
	if internal, ok := m.InternalFor[external]; ok {
		return internal
	}
	return external
}

// Build produces a Mapping for the given internal tool names under the
// supplied Policy. The returned mapping is deterministic: two
// invocations with the same name set and the same Policy produce
// identical external names and identical collision detection.
//
// Build returns an error when two distinct internal names normalise
// to the same external name and the disambiguation suffix cannot
// resolve the collision (e.g. caller passed the same internal name
// twice). Failing closed is the correct behaviour for the loop: a
// silent alias would route a tool call to the wrong handler.
func Build(internalNames []string, policy Policy) (*Mapping, error) {
	// First pass: compute the candidate external name for every internal
	// name by normalising it onto the policy's character set. The
	// candidate slice is positionally aligned with internalNames, so the
	// collision resolver below can key disambiguation off the internal
	// name while colliding on the candidate.
	candidates := make([]string, len(internalNames))
	for i, name := range internalNames {
		candidates[i] = sanitize(name, policy)
	}
	return BuildFromCandidates(internalNames, candidates, policy)
}

// BuildFromCandidates resolves collisions among caller-supplied external
// candidate names and returns the round-trip Mapping. It is the shared
// core of the collision algorithm: Build calls it after sanitising each
// internal name onto the policy character set, and the toolset-profile
// presenter (issue #234) calls it with each tool's profile alias as the
// candidate so alias collisions are disambiguated by exactly the same
// deterministic hash-suffix scheme — there is no second algorithm.
//
// keys are the unique identities the disambiguation suffix is derived
// from (the internal tool IDs); candidates[i] is the desired external
// name for keys[i]. The two slices must be the same length. A duplicate
// key is a caller bug (the registry contract is that internal names are
// unique) and is rejected; two distinct keys whose candidates collide are
// disambiguated by appending the SHA-256-derived suffix of the colliding
// key, within the policy's length budget. A collision the suffix cannot
// resolve fails closed — a silent alias would route a tool call to the
// wrong handler.
func BuildFromCandidates(keys, candidates []string, policy Policy) (*Mapping, error) {
	if len(keys) != len(candidates) {
		return nil, fmt.Errorf("toolname: keys/candidates length mismatch (%d vs %d)", len(keys), len(candidates))
	}

	m := &Mapping{
		ExternalFor: make(map[string]string, len(keys)),
		InternalFor: make(map[string]string, len(keys)),
	}

	// Deduplicate keys — a single internal name passed twice is a caller
	// bug, not a collision. The registry contract is that names are
	// unique, but tests and embedding callers may bypass it; surface the
	// duplicate as an explicit error rather than silently overwriting the
	// first entry.
	seen := make(map[string]struct{}, len(keys))
	for _, n := range keys {
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("toolname: duplicate internal tool name %q", n)
		}
		seen[n] = struct{}{}
	}

	// Detect collisions among the candidates. When two candidates collide,
	// derive a disambiguating suffix from the colliding key's SHA-256
	// hash. This is deterministic, keeps the suffix short, and survives
	// reordering of the input slice.
	//
	// The suffix is appended within the MaxLen budget — a candidate may be
	// shorter than MaxLen, in which case the suffix is added directly; a
	// MaxLen-truncated candidate has its truncation shortened to make
	// room.
	used := make(map[string]string, len(candidates)) // external → key
	for i, ext := range candidates {
		key := keys[i]
		if existing, taken := used[ext]; taken && existing != key {
			ext = disambiguate(ext, key, policy)
			if other, stillCollides := used[ext]; stillCollides && other != key {
				return nil, fmt.Errorf(
					"toolname: cannot resolve collision between %q and %q (both normalise to %q under policy MaxLen=%d AllowHyphen=%v AllowLeadingDigit=%v)",
					key, other, ext, policy.MaxLen, policy.AllowHyphen, policy.AllowLeadingDigit,
				)
			}
		}
		used[ext] = key
		m.ExternalFor[key] = ext
		m.InternalFor[ext] = key
	}

	return m, nil
}

// BuildSorted is a convenience wrapper for callers that hold a
// ToolDefinition-like slice and want a deterministic Mapping
// independent of registration order. The caller may want this when
// the registry's order is operator-controlled (e.g. via a config
// file) but the Mapping is consumed by trace persistence where order
// drift between runs would inflate diffs.
func BuildSorted(internalNames []string, policy Policy) (*Mapping, error) {
	names := make([]string, len(internalNames))
	copy(names, internalNames)
	sort.Strings(names)
	return Build(names, policy)
}

// sanitize lowers a single name onto the policy's character set and
// length budget. It does not handle collisions — Build composes
// sanitize with disambiguate above.
func sanitize(name string, policy Policy) string {
	if name == "" {
		// An empty internal name is a registration bug, but reach this
		// branch defensively rather than emit "" to the wire (every
		// provider rejects it). Use a stable placeholder so collision
		// detection can still flag the duplicate if it happens twice.
		return "_unnamed"
	}

	var b strings.Builder
	b.Grow(len(name))
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 && !policy.AllowLeadingDigit {
				// Prepend an underscore the first time we see a
				// leading digit; the digit itself is still preserved.
				b.WriteByte('_')
			}
			b.WriteRune(r)
		case r == '_':
			b.WriteRune(r)
		case r == '-' && policy.AllowHyphen:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	out := b.String()

	// Enforce length cap. A name that exceeds MaxLen is hard-truncated
	// here; Build's disambiguation pass adds a hash suffix when the
	// truncated form collides with another truncated form.
	if policy.MaxLen > 0 && len(out) > policy.MaxLen {
		out = out[:policy.MaxLen]
	}
	return out
}

// disambiguate produces a deterministic disambiguating form of ext for
// the given internal name, keeping the result within the policy's
// length budget. The suffix is derived from the SHA-256 of the
// internal name so the same internal name always maps to the same
// disambiguated form regardless of the order in which collisions are
// detected.
//
// Suffix layout: "_" + first 6 hex chars of SHA-256(internal). 7
// characters total. When MaxLen is short enough that suffix + truncated
// prefix do not fit, the prefix is trimmed further to make room.
func disambiguate(ext, internal string, policy Policy) string {
	const suffixHexChars = 6
	suffix := nameHashSuffix(internal, suffixHexChars)
	prefix := ext

	if policy.MaxLen > 0 {
		budget := policy.MaxLen - len(suffix)
		if budget < 1 {
			// Pathological MaxLen — return just the suffix so we still
			// produce a deterministic name. Build's caller-side
			// collision detection will then surface this as a hard
			// error if it cannot be made unique.
			return suffix[:min(len(suffix), policy.MaxLen)]
		}
		if len(prefix) > budget {
			prefix = prefix[:budget]
		}
	}

	return prefix + suffix
}

// nameHashSuffix returns "_" followed by the first hexChars hex
// characters of the SHA-256 digest of name. Used by disambiguate to
// generate a deterministic, collision-resistant tag.
func nameHashSuffix(name string, hexChars int) string {
	sum := sha256.Sum256([]byte(name))
	enc := hex.EncodeToString(sum[:])
	if hexChars > len(enc) {
		hexChars = len(enc)
	}
	return "_" + enc[:hexChars]
}

// Package toolname provides per-provider normalization and collision
// detection for tool/function names that flow from the registry into a
// provider request. See docs/architecture.md#provider-facing-tool-name-normalization
// for the design and per-provider rules.
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
// during normalization: MaxLen bounds the externalised name, and
// AllowHyphen / AllowLeadingDigit toggle the two real differences across
// the providers Stirrup targets today.
type Policy struct {
	MaxLen            int
	AllowHyphen       bool
	AllowLeadingDigit bool
}

// PolicyFor returns the Policy for the given provider type (the
// discriminator used in RunConfig.Provider.Type). Unknown providers fall
// through to the strictest policy.
//
// TODO(#221): move the per-provider data into the capability profile so
// each adapter declares its rules in one place.
func PolicyFor(providerType string) Policy {
	switch providerType {
	case "anthropic":
		// [A-Za-z0-9_-]{1,64}, leading digits allowed.
		return Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	case "openai-compatible", "openai-responses":
		// Same character set as Anthropic.
		return Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	case "bedrock":
		// Conservative union of the Anthropic/OpenAI-backed models it fronts.
		return Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	case "gemini":
		// Vertex AI: no hyphens, no leading digit.
		return Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: false}
	default:
		// Strictest policy until a real one is registered.
		return Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: false}
	}
}

// Mapping records the bidirectional translation between internal
// (registry-side) names and external (provider-facing) names for a
// single request. A name absent from ExternalFor is one the caller never
// passed in and round-trips unchanged rather than erroring.
type Mapping struct {
	// ExternalFor maps internal name → external name.
	ExternalFor map[string]string
	// InternalFor is the inverse, consulted when translating an inbound
	// tool_call event back to the registry-side name.
	InternalFor map[string]string
}

// Translate returns the external name for an internal one, falling
// back to the input when the name is not in the mapping.
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
// to the input on a miss.
func (m *Mapping) Reverse(external string) string {
	if m == nil {
		return external
	}
	if internal, ok := m.InternalFor[external]; ok {
		return internal
	}
	return external
}

// Build produces a deterministic Mapping for the given internal tool
// names under the supplied Policy. It returns an error when a collision
// after normalization cannot be resolved by disambiguation — failing
// closed rather than silently aliasing a tool call to the wrong handler.
func Build(internalNames []string, policy Policy) (*Mapping, error) {
	candidates := make([]string, len(internalNames))
	for i, name := range internalNames {
		candidates[i] = sanitize(name, policy)
	}
	return BuildFromCandidates(internalNames, candidates, policy)
}

// BuildFromCandidates resolves collisions among caller-supplied external
// candidate names and returns the round-trip Mapping. It is the shared
// collision core: Build calls it after sanitising each internal name,
// and the toolset-profile presenter calls it with each tool's alias as
// the candidate, so alias collisions resolve via the same scheme.
//
// keys are the unique identities the disambiguation suffix is derived
// from; candidates[i] is the desired external name for keys[i]. The two
// slices must be the same length. A duplicate key is rejected; distinct
// keys whose candidates collide are disambiguated with a SHA-256-derived
// suffix of the colliding key, within the policy's length budget. A
// collision the suffix cannot resolve fails closed.
func BuildFromCandidates(keys, candidates []string, policy Policy) (*Mapping, error) {
	if len(keys) != len(candidates) {
		return nil, fmt.Errorf("toolname: keys/candidates length mismatch (%d vs %d)", len(keys), len(candidates))
	}

	m := &Mapping{
		ExternalFor: make(map[string]string, len(keys)),
		InternalFor: make(map[string]string, len(keys)),
	}

	// A duplicate key is a caller bug, not a collision, since the registry
	// contract is that internal names are unique.
	seen := make(map[string]struct{}, len(keys))
	for _, n := range keys {
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("toolname: duplicate internal tool name %q", n)
		}
		seen[n] = struct{}{}
	}

	used := make(map[string]string, len(candidates)) // external → key
	for i, ext := range candidates {
		key := keys[i]
		if existing, taken := used[ext]; taken && existing != key {
			// Capture the pre-disambiguation candidate so an irresolvable
			// error names the alias the author actually wrote.
			origExt := ext
			ext = disambiguate(ext, key, policy)
			if other, stillCollides := used[ext]; stillCollides && other != key {
				return nil, fmt.Errorf(
					"toolname: cannot resolve collision between %q and %q (both normalise to %q under policy MaxLen=%d AllowHyphen=%v AllowLeadingDigit=%v)",
					key, other, origExt, policy.MaxLen, policy.AllowHyphen, policy.AllowLeadingDigit,
				)
			}
		}
		used[ext] = key
		m.ExternalFor[key] = ext
		m.InternalFor[ext] = key
	}

	return m, nil
}

// BuildSorted builds a Mapping independent of input ordering — useful
// when the registry order is operator-controlled but the Mapping feeds
// trace persistence, where order drift between runs would inflate diffs.
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

	// Build's disambiguation pass adds a hash suffix when a truncated
	// form collides with another truncated form.
	if policy.MaxLen > 0 && len(out) > policy.MaxLen {
		out = out[:policy.MaxLen]
	}
	return out
}

// disambiguate produces a deterministic disambiguating form of ext for
// the given internal name, within the policy's length budget. The
// suffix ("_" + 6 hex chars of SHA-256(internal)) depends only on the
// internal name, so the result is independent of collision-detection
// order.
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

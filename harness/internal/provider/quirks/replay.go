package quirks

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file implements the shared ReplayFields path parser and walker.
// ReplayFields is the design D12 surface: a rule lists assistant-message
// field paths that should be captured from a provider response so the
// agentic loop can echo them back on the next turn. Wave 2 lands
// parse-side recognition only (design §9 risk 7) — the captured values
// surface in trace attributes so an operator can see the rule fired,
// but they are not yet threaded back into outbound message history.
//
// The parser is cross-adapter: both openai.go and gemini.go call it
// against their decoded SSE payloads, so the path syntax is the only
// place that needs to stay coherent across providers. A rule author
// who writes "candidates[].content.parts[].functionCall.thoughtSignature"
// for Gemini and "reasoning_content" for DeepSeek can rely on the same
// semantics: dot-separated keys descend objects, `[]` iterates arrays
// of objects.
//
// Path grammar (informal):
//
//   path     := segment ( "." segment )*
//   segment  := key ( "[]" )?
//   key      := [A-Za-z_][A-Za-z0-9_]*
//
// `[]` always means "iterate every element". A path that ends in `[]`
// would name a value rather than a key, which is meaningless for
// ReplayFields (the surface is keyed on field names), so a trailing
// `[]` is a syntax error. Indexed access (`[0]`) is intentionally NOT
// supported in v1: a rule that wants to pin a single index is almost
// certainly modelling provider behaviour wrong (e.g. assuming Gemini
// only ever returns one candidate).
//
// The parser is strict: bad paths return an error from
// ValidateReplayPath, called from BuiltinRulesValidate, so a typo in a
// rule fails the test rather than silently capturing nothing at parse
// time. A bad path that somehow reaches the walker logs at debug and
// captures nothing, matching the "loudly elsewhere, safe at runtime"
// pattern used by RuleMatches.

// ReplayPathSegment is one decoded segment of a ReplayFields path.
// Key is the JSON field name; IsArray reports whether the segment is
// followed by `[]`, meaning the walker should iterate the value as an
// array of objects rather than treating it as the terminal value.
type ReplayPathSegment struct {
	Key     string
	IsArray bool
}

// ParseReplayPath decodes one path string into its ordered segments.
// Returns an error when the input is empty, contains an empty segment
// ("a..b"), uses an unsupported character in a key, or ends with `[]`
// (which would name a value, not a field).
//
// The parser is intentionally small and table-driven so the surface
// stays auditable. Callers should treat the returned segments as
// immutable — the walker reads them many times per response.
func ParseReplayPath(path string) ([]ReplayPathSegment, error) {
	if path == "" {
		return nil, fmt.Errorf("quirks: empty replay path")
	}
	rawSegments := strings.Split(path, ".")
	out := make([]ReplayPathSegment, 0, len(rawSegments))
	for i, raw := range rawSegments {
		if raw == "" {
			return nil, fmt.Errorf("quirks: empty segment at position %d in replay path %q", i, path)
		}
		seg := ReplayPathSegment{Key: raw}
		if strings.HasSuffix(raw, "[]") {
			seg.IsArray = true
			seg.Key = strings.TrimSuffix(raw, "[]")
			if seg.Key == "" {
				return nil, fmt.Errorf("quirks: bare [] without a key at position %d in replay path %q", i, path)
			}
		}
		if !isValidReplayKey(seg.Key) {
			return nil, fmt.Errorf("quirks: invalid key %q at position %d in replay path %q", seg.Key, i, path)
		}
		out = append(out, seg)
	}
	// A trailing `[]` would mean "iterate the values at the terminal
	// position" — meaningless for a field-capture surface. Forbid it so
	// rule authors notice the typo at registry-build time rather than
	// at runtime when nothing is captured.
	if out[len(out)-1].IsArray {
		return nil, fmt.Errorf("quirks: replay path %q ends in [] — paths must terminate on a field name, not an array iteration", path)
	}
	return out, nil
}

// ValidateReplayPath reports whether the supplied path is syntactically
// valid. Used by BuiltinRulesValidate to fail the build on a rule that
// names a malformed path; the walker does not need to re-validate
// because parsed paths are cached at adapter startup.
func ValidateReplayPath(path string) error {
	_, err := ParseReplayPath(path)
	return err
}

// isValidReplayKey reports whether s is a syntactically valid JSON
// field-name segment under the ReplayFields grammar. Restricted to
// `[A-Za-z_][A-Za-z0-9_]*` so the path syntax has no overlap with the
// reserved `.` separator or the `[]` array marker. Provider field names
// that violate this set (e.g. a literal dot in a key) cannot be
// expressed; if a real upstream emits such a key, the grammar needs a
// quoting form before the rule can reference it.
func isValidReplayKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// CaptureReplayFields walks a decoded JSON document (a map or slice
// produced by encoding/json into `any`) and returns the value at each
// supplied path. Paths that do not resolve are silently skipped —
// missing fields are the common case (a v3 thoughtSignature on a v2.5
// response, for instance), not an error.
//
// The returned map is keyed by the original path string (verbatim from
// ReplayFields), so consumers can render trace attributes without
// re-deriving the source key. Map iteration order is non-deterministic
// in Go; callers that need stable ordering should sort the keys
// themselves.
//
// Each captured value is the raw decoded JSON value (`string`,
// `float64`, `map[string]any`, etc.). Trace consumers stringify
// downstream as appropriate. For paths with `[]` segments, multiple
// values may exist; the returned slice holds them in walk order.
//
// CaptureReplayFields is intentionally tolerant of malformed paths at
// runtime: a path that fails ParseReplayPath is silently dropped
// rather than panicking. The rule registry's build-time validator
// catches the malformed path; if it slips through, runtime stays safe.
func CaptureReplayFields(doc any, paths []string) map[string][]any {
	if len(paths) == 0 {
		return nil
	}
	out := map[string][]any{}
	for _, raw := range paths {
		segments, err := ParseReplayPath(raw)
		if err != nil {
			continue
		}
		values := walkReplayPath(doc, segments)
		if len(values) > 0 {
			out[raw] = values
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// walkReplayPath recursively descends doc according to segments.
// At each step, an array-marked segment iterates every element of the
// value at the key; a non-array segment descends into a single value.
// Returns the collected leaf values in walk order; a path that fails
// to resolve returns nil.
func walkReplayPath(doc any, segments []ReplayPathSegment) []any {
	if len(segments) == 0 {
		// Terminal: return the document itself. The non-empty path
		// invariant on ParseReplayPath means this branch is only
		// reached after at least one segment has been consumed.
		if doc == nil {
			return nil
		}
		return []any{doc}
	}
	seg := segments[0]
	rest := segments[1:]

	obj, ok := doc.(map[string]any)
	if !ok {
		return nil
	}
	val, ok := obj[seg.Key]
	if !ok || val == nil {
		return nil
	}

	if !seg.IsArray {
		return walkReplayPath(val, rest)
	}
	arr, ok := val.([]any)
	if !ok {
		return nil
	}
	var collected []any
	for _, el := range arr {
		collected = append(collected, walkReplayPath(el, rest)...)
	}
	return collected
}

// CaptureFromJSON is a convenience wrapper around CaptureReplayFields
// that takes raw JSON bytes. Used by the openai adapter's parse hook,
// which has a json.RawMessage in hand rather than a decoded map.
// Returns nil on a decode error — the SSE parser is the authoritative
// reporter for chunk-level decode failures; this wrapper does not
// duplicate that error path.
func CaptureFromJSON(raw json.RawMessage, paths []string) map[string][]any {
	if len(raw) == 0 || len(paths) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return CaptureReplayFields(doc, paths)
}

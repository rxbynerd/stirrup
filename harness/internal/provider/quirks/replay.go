package quirks

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file implements the shared ReplayFields path parser and walker
// used by both the openai and gemini adapters. See
// docs/provider-quirks.md §2.3 for the path grammar, the
// "(threaded)" / "(parse-side only)" Description convention, and the
// rationale for omitting indexed ([N]) access.

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
// Precondition: paths come from first-party BuiltinRules() only, so no
// segment-count or key-length cap is enforced here. An
// operator-injectable path surface would need one at the injection
// point.
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

// isValidReplayKey restricts keys to `[A-Za-z_][A-Za-z0-9_]*` so the path
// syntax has no overlap with the `.` separator or `[]` array marker. A
// provider field name with a literal dot cannot be expressed without a
// future quoting form.
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
// produced by encoding/json into `any`) and returns the value(s) at
// each supplied path, keyed by the original path string. Paths that do
// not resolve, or that fail ParseReplayPath, are silently skipped
// rather than erroring — missing fields are the common case (e.g. a v3
// thoughtSignature on a v2.5 response). Map iteration order is
// non-deterministic; callers needing stable ordering must sort keys
// themselves.
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
// that takes raw JSON bytes. Returns nil on a decode error rather than
// erroring — the SSE parser is the authoritative reporter for
// chunk-level decode failures.
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

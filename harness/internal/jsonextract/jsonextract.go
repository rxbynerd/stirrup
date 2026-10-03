// Package jsonextract locates JSON objects embedded in free-form model
// output, such as a classifier or judge verdict that follows prose.
package jsonextract

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// maxScanBytes bounds the bytes one extraction examines across all
// candidate objects, so output with many unbalanced braces cannot make
// extraction quadratic.
const maxScanBytes = 4 << 20

// NonceKey is the member that binds a verdict object to one prompt.
const NonceKey = "nonce"

var (
	// ErrEmptyNonce rejects an empty nonce, which content could match
	// with "nonce": "" without knowing anything about the prompt.
	ErrEmptyNonce = errors.New("jsonextract: empty nonce")

	// ErrNoNonceObject reports that no top-level object carries the nonce.
	ErrNoNonceObject = errors.New("jsonextract: no top-level object carries the nonce")

	// ErrConflictingObjects reports two objects that carry the nonce but
	// differ in their members.
	ErrConflictingObjects = errors.New("jsonextract: objects carrying the nonce differ")

	// ErrScanBudget reports that maxScanBytes ran out before the input was
	// fully examined. It is returned even when a match was already seen,
	// so a verdict is never chosen from a partial scan.
	ErrScanBudget = errors.New("jsonextract: scan budget exhausted")
)

// ObjectWithNonce returns the members of the top-level JSON object in s
// whose NonceKey member is the JSON string nonce.
//
// Top-level means not nested inside another brace pair; braces inside
// JSON strings are text. A balanced brace pair that is not valid JSON is
// skipped whole, so an object quoted inside it (for example in a
// reasoning string with unescaped quotes) is never a candidate; an
// unterminated '{' is skipped by one byte. Objects without the nonce, or
// with another value, are ignored and never conflict. Objects carrying
// the nonce must all be identical, else ErrConflictingObjects. Member
// lookup is exact, and a key repeated within one object takes its last
// value, as in encoding/json.
func ObjectWithNonce(s, nonce string) (map[string]json.RawMessage, error) {
	if nonce == "" {
		return nil, ErrEmptyNonce
	}
	var (
		match    map[string]json.RawMessage
		matchRaw []byte
	)
	budget := maxScanBytes
	for i := 0; i < len(s); {
		off := strings.IndexByte(s[i:], '{')
		if off < 0 {
			break
		}
		start := i + off
		end, scanned := matchBrace(s, start, budget)
		budget -= scanned
		if end < 0 {
			if budget <= 0 {
				return nil, ErrScanBudget
			}
			i = start + 1
			continue
		}
		i = end + 1

		candidate := []byte(s[start : end+1])
		var members map[string]json.RawMessage
		if err := json.Unmarshal(candidate, &members); err != nil {
			continue
		}
		var got string
		if err := json.Unmarshal(members[NonceKey], &got); err != nil || got != nonce {
			continue
		}
		if match == nil {
			match, matchRaw = members, candidate
			continue
		}
		if !sameJSON(matchRaw, candidate) {
			return nil, ErrConflictingObjects
		}
	}
	if match == nil {
		return nil, ErrNoNonceObject
	}
	return match, nil
}

// sameJSON reports whether a and b decode to equal values, ignoring
// whitespace and member order.
func sameJSON(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// matchBrace returns the index of the '}' closing the '{' at s[start],
// treating braces inside JSON strings as text, and the number of bytes it
// examined. The index is -1 when the object is unterminated or budget
// bytes were examined without finding the close.
func matchBrace(s string, start, budget int) (int, int) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		if i-start >= budget {
			return -1, budget
		}
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, i - start + 1
			}
		}
	}
	return -1, len(s) - start
}

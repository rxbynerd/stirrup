// Package jsonextract locates JSON objects embedded in free-form model
// output, such as a classifier or judge verdict that follows prose.
package jsonextract

import (
	"encoding/json"
	"strings"
)

// maxScanBytes bounds the bytes LastObjectWithKey examines across all
// candidate objects, so output with many unbalanced braces cannot make
// extraction quadratic. Exhausting it reports no object.
const maxScanBytes = 4 << 20

// LastObjectWithKey returns the members of the last top-level JSON object
// in s that has key as a member. Top-level means not nested inside another
// valid JSON object; brace pairs in prose that do not parse as JSON are
// skipped, and braces inside JSON strings are treated as text. Member
// lookup is exact, unlike encoding/json's case-insensitive struct
// decoding.
//
// Callers that evaluate untrusted content want the last object: the
// content precedes the instruction to answer, so an object planted in it
// appears before the model's own reply.
func LastObjectWithKey(s, key string) (map[string]json.RawMessage, bool) {
	var last map[string]json.RawMessage
	budget := maxScanBytes
	for i := 0; i < len(s); {
		off := strings.IndexByte(s[i:], '{')
		if off < 0 {
			break
		}
		start := i + off
		end, scanned := matchBrace(s, start, budget)
		budget -= scanned
		if budget <= 0 {
			return nil, false
		}
		if end < 0 {
			i = start + 1
			continue
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s[start:end+1]), &members); err != nil {
			i = start + 1
			continue
		}
		if _, ok := members[key]; ok {
			last = members
		}
		i = end + 1
	}
	return last, last != nil
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

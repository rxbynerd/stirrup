package judge

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// nonceScanBudget bounds the bytes the brace scan reads in total; an
// unterminated candidate is rescanned from its next brace, so without a
// budget a reply of open braces costs quadratic time.
const nonceScanBudget = 4 << 20

const nonceKey = "nonce"

var (
	errEmptyNonce              = errors.New("empty nonce")
	errNoNonceObject           = errors.New("no top-level JSON object in the model reply carries this call's nonce")
	errConflictingNonceObjects = errors.New("JSON objects in the model reply that carry this call's nonce differ")
	errNonceScanBudget         = errors.New("model reply exhausted the JSON scan budget")
)

// nonceObject is the top-level JSON object in a reply that carries a
// nonce.
type nonceObject struct {
	members map[string]json.RawMessage

	// raw is the first matching object's text.
	raw string

	// matches counts matching objects; all are identical as JSON values.
	matches int

	// candidates counts balanced top-level brace spans, valid JSON or not.
	candidates int
}

// findNonceObject reimplements harness/internal/jsonextract.ObjectWithNonce,
// which eval cannot import; testdata/nonce_extraction_vectors.json pins both
// to the same rules. A balanced candidate that is not a JSON object whose
// "nonce" member is the string nonce is skipped whole; an unterminated one
// is rescanned from its next brace. Matching objects that differ are an
// error, and so is exhausting the scan budget, even after a match. The
// returned candidates count is valid on every path.
func findNonceObject(s, nonce string) (nonceObject, error) {
	var found nonceObject
	if nonce == "" {
		return found, errEmptyNonce
	}
	budget := nonceScanBudget
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
				return found, errNonceScanBudget
			}
			i = start + 1
			continue
		}
		i = end + 1
		found.candidates++

		candidate := s[start : end+1]
		var members map[string]json.RawMessage
		if err := json.Unmarshal([]byte(candidate), &members); err != nil {
			continue
		}
		var got string
		if err := json.Unmarshal(members[nonceKey], &got); err != nil || got != nonce {
			continue
		}
		if found.matches == 0 {
			found.members, found.raw = members, candidate
		} else if !sameJSON(found.raw, candidate) {
			return found, errConflictingNonceObjects
		}
		found.matches++
	}
	if found.matches == 0 {
		return found, errNoNonceObject
	}
	return found, nil
}

// sameJSON reports whether a and b decode to equal JSON values.
func sameJSON(a, b string) bool {
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// matchBrace returns the index of the brace closing the one at s[start],
// skipping braces inside JSON strings, and the bytes it read. The index is
// -1 when the object never closes or budget runs out first.
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

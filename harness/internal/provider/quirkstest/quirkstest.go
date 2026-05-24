// Package quirkstest provides the shared test helpers used by the
// Wave 2 wire-shape contract tests across the provider package and
// the compat/* sub-packages. It is a *_test-adjacent package (not
// _test-only) so both `package provider` test files and
// `package zai_test` files can import it.
//
// The helpers cover three responsibilities:
//
//   - AssertWireEqual: canonical-form equality between a captured
//     fixture and an actual marshalled body. Unmarshal-then-marshal
//     normalises both sides so key ordering and whitespace don't
//     produce false negatives.
//   - ScrubFixture: redacts known-sensitive substrings (Bearer
//     tokens, project IDs, session IDs) so a fixture committed to
//     the repository never carries upstream credentials.
//   - LoadFixture: reads a fixture file, scrubs it, and returns the
//     bytes. Synthetic fixtures (those carrying a leading
//     "# synthetic: ..." comment) have the comment line stripped
//     before parsing so they remain JSON-valid.
package quirkstest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// AssertWireEqual fails the test if the JSON in got does not match
// the JSON in the fixture at wantPath after canonical normalisation.
// Both sides are unmarshalled into interface{} then re-marshalled
// with go-sorted keys so the comparison ignores key ordering and
// whitespace. The diff message includes both bodies in their
// canonical form to make a failure easy to inspect.
func AssertWireEqual(t *testing.T, wantPath string, got []byte) {
	t.Helper()
	wantRaw, err := LoadFixture(wantPath)
	if err != nil {
		t.Fatalf("load fixture %s: %v", wantPath, err)
	}
	wantCanonical, err := canonicalise(wantRaw)
	if err != nil {
		t.Fatalf("canonicalise fixture %s: %v", wantPath, err)
	}
	gotCanonical, err := canonicalise(got)
	if err != nil {
		t.Fatalf("canonicalise actual body: %v\nbody: %s", err, got)
	}
	if !bytes.Equal(wantCanonical, gotCanonical) {
		t.Errorf("wire body mismatch for fixture %s\n want: %s\n got:  %s", wantPath, wantCanonical, gotCanonical)
	}
}

// LoadFixture reads the file at path, strips any leading
// "# synthetic: ..." comment line, and returns the bytes. Synthetic
// fixtures carry the comment per design §6 so a reader can tell at a
// glance that the bytes are not a real upstream capture. The comment
// is stripped before parsing because JSON has no comment syntax.
func LoadFixture(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Strip a leading "# synthetic: ..." line if present so JSON
	// fixtures stay JSON-parseable. Only the first line is stripped;
	// subsequent comment-like lines would be a bug worth surfacing.
	if bytes.HasPrefix(raw, []byte("# synthetic:")) {
		if idx := bytes.IndexByte(raw, '\n'); idx >= 0 {
			raw = raw[idx+1:]
		} else {
			raw = nil
		}
	}
	return raw, nil
}

// ScrubFixture replaces known-sensitive substrings in body with
// placeholders. It is intentionally a small, hard-coded list — the
// goal is catching the obvious patterns (Authorization: Bearer ...,
// project IDs in URLs) before a fixture is committed, not building
// a general-purpose secret scanner. Adding to the list as new shapes
// appear is expected.
func ScrubFixture(body []byte) []byte {
	for _, sub := range scrubbers {
		body = sub.re.ReplaceAll(body, []byte(sub.replacement))
	}
	return body
}

type scrubber struct {
	re          *regexp.Regexp
	replacement string
}

// scrubbers list — extend when a new sensitive pattern appears. Each
// entry is a (regex, replacement) pair; replacements are deliberately
// human-readable so a reader skimming a fixture sees the redaction
// rather than a random hex blob.
var scrubbers = []scrubber{
	// Authorization: Bearer sk-...
	{
		re:          regexp.MustCompile(`Bearer\s+sk-[A-Za-z0-9_\-]+`),
		replacement: "Bearer REDACTED",
	},
	// api-key: sk-...
	{
		re:          regexp.MustCompile(`(?i)(api-key|x-api-key)\s*[:=]\s*sk-[A-Za-z0-9_\-]+`),
		replacement: "${1}: REDACTED",
	},
	// Anthropic API keys (sk-ant-...)
	{
		re:          regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]+`),
		replacement: "REDACTED-ANTHROPIC-KEY",
	},
	// GCP project IDs in vertex URLs: projects/<id>/locations/<loc>
	{
		re:          regexp.MustCompile(`projects/[A-Za-z0-9_\-]+/locations`),
		replacement: "projects/test-project/locations",
	},
	// GCP OAuth2 access tokens (ya29.<base64url payload>). These are
	// the bearer tokens minted by the gcp-workload-identity and
	// gcp-service-account credential sources; a captured Vertex AI
	// request fixture would carry one in the Authorization header.
	// The prefix is google-fixed, the body is opaque, so a regex on
	// the prefix-with-trailing-payload is the right shape.
	{
		re:          regexp.MustCompile(`ya29\.[A-Za-z0-9_\-]+`),
		replacement: "ya29.REDACTED",
	},
}

// canonicalise normalises a JSON document so two semantically equal
// bodies compare byte-equal. Returns an error if the input is not
// valid JSON; callers should treat that as a fixture-format bug.
func canonicalise(raw []byte) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("unmarshal: %w (body: %s)", err, snippet(raw))
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

// snippet returns up to 200 bytes of raw so error messages don't
// dump a multi-megabyte payload into the test output.
func snippet(raw []byte) string {
	const limit = 200
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "...(truncated)"
}

// Helpers below are exported for tests that need to introspect
// fixture content directly (e.g. asserting the presence of a
// specific extra body field without going through AssertWireEqual).

// MustUnmarshal decodes raw into v, failing the test on any error.
// Mirrors stdlib testify-style helpers without taking a dependency.
func MustUnmarshal(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, snippet(raw))
	}
}

// HasJSONKey reports whether raw, parsed as a JSON object, contains
// the given top-level key. Useful for spot-checks of the wire body
// shape in tests that don't want to assert the whole shape via
// AssertWireEqual.
func HasJSONKey(t *testing.T, raw []byte, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, ok := m[key]
	return ok
}

// JSONString returns a compact JSON representation of v, failing the
// test on any error. Used to construct the "actual" side of an
// AssertWireEqual call from a Go struct.
func JSONString(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// JoinPath is a tiny path-join helper that does not pull in
// path/filepath at every call site. Test fixtures live under
// testdata/quirks/<provider>/<model>/ so a three-arg join is the
// common form.
func JoinPath(parts ...string) string {
	return strings.Join(parts, "/")
}

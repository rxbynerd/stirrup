// Package quirkstest provides shared test helpers for wire-shape contract
// tests across the provider package and the compat/* sub-packages. It is a
// *_test-adjacent package (not _test-only) so both `package provider` test
// files and `package zai_test` files can import it.
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

// AssertWireEqual fails the test if the JSON in got does not match the
// JSON in the fixture at wantPath after canonical normalisation (so key
// ordering and whitespace don't produce false negatives).
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
// fixtures carry the comment so a reader can tell the bytes are not a
// real upstream capture; the comment is stripped before parsing since
// JSON has no comment syntax.
func LoadFixture(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

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
// placeholders. It is intentionally a small, hard-coded list, not a
// general-purpose secret scanner.
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

// scrubbers list — extend when a new sensitive pattern appears.
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
	// GCP OAuth2 access tokens (ya29.<base64url payload>), e.g. from
	// gcp-workload-identity / gcp-service-account credential sources.
	{
		re:          regexp.MustCompile(`ya29\.[A-Za-z0-9_\-]+`),
		replacement: "ya29.REDACTED",
	},
}

// canonicalise normalises a JSON document so two semantically equal
// bodies compare byte-equal.
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

// snippet returns up to 200 bytes of raw so error messages stay short.
func snippet(raw []byte) string {
	const limit = 200
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "...(truncated)"
}

// MustUnmarshal decodes raw into v, failing the test on any error.
func MustUnmarshal(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, snippet(raw))
	}
}

// HasJSONKey reports whether raw, parsed as a JSON object, contains
// the given top-level key.
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
// test on any error.
func JSONString(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// JoinPath is a tiny path-join helper that avoids pulling in
// path/filepath at every call site.
func JoinPath(parts ...string) string {
	return strings.Join(parts, "/")
}

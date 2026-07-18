package quirkstest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
)

// TestScrubFixture pins the substitution behaviour of each scrubber rule.
// Adding a scrubber to the package's `scrubbers` list requires a
// corresponding row here.
func TestScrubFixture(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "openai bearer token",
			in:   "Authorization: Bearer sk-1234567890abcdef",
			want: "Authorization: Bearer REDACTED",
		},
		{
			name: "x-api-key header",
			in:   "x-api-key: sk-abcdef1234567890",
			want: "x-api-key: REDACTED",
		},
		{
			name: "anthropic api key",
			in:   "sk-ant-api03-abcdef1234567890",
			want: "REDACTED-ANTHROPIC-KEY",
		},
		{
			name: "gcp project id in vertex url",
			in:   "https://aiplatform.googleapis.com/v1/projects/prod-billing-1234/locations/us-central1/...",
			want: "https://aiplatform.googleapis.com/v1/projects/test-project/locations/us-central1/...",
		},
		{
			name: "gcp ya29 oauth token",
			in:   "Authorization: Bearer ya29.AHES6ZS-7n8abcdefghijklmnop",
			want: "Authorization: Bearer ya29.REDACTED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(quirkstest.ScrubFixture([]byte(tc.in)))
			if !strings.Contains(got, tc.want) {
				t.Errorf("ScrubFixture(%q) = %q, want substring %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFixturesScrubbed is the CI gate ensuring no fixture committed under
// harness/internal/provider/testdata/quirks/ carries an upstream credential
// or other sensitive substring: it asserts ScrubFixture is a no-op against
// every fixture's content.
func TestFixturesScrubbed(t *testing.T) {
	fixtureRoot := filepath.Join("..", "testdata", "quirks")
	if _, err := os.Stat(fixtureRoot); err != nil {
		t.Skipf("fixture root %q unavailable: %v", fixtureRoot, err)
	}
	checked := 0
	err := filepath.Walk(fixtureRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scrubbed := quirkstest.ScrubFixture(raw)
		if !bytes.Equal(raw, scrubbed) {
			diff := firstDiff(raw, scrubbed)
			t.Errorf("fixture %s contains a substring the scrubber would rewrite (first divergence at offset %d): "+
				"raw=%q scrubbed=%q. Commit the scrubbed form instead, or extend the scrubber list if the substring is benign.",
				path, diff.offset, diff.rawSnippet, diff.scrubbedSnippet)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixtures: %v", err)
	}
	if checked == 0 {

		t.Fatalf("walked %s and found zero fixtures; the gate is no longer enforcing anything", fixtureRoot)
	}
}

// diff surfaces the first byte at which raw and scrubbed diverge.
type diff struct {
	offset          int
	rawSnippet      string
	scrubbedSnippet string
}

func firstDiff(a, b []byte) diff {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return diff{
				offset:          i,
				rawSnippet:      snippet(a, i),
				scrubbedSnippet: snippet(b, i),
			}
		}
	}
	if len(a) != len(b) {
		return diff{offset: n, rawSnippet: snippet(a, n), scrubbedSnippet: snippet(b, n)}
	}
	return diff{}
}

func snippet(s []byte, around int) string {
	const window = 40
	start := around - window
	if start < 0 {
		start = 0
	}
	end := around + window
	if end > len(s) {
		end = len(s)
	}
	return strings.ReplaceAll(string(s[start:end]), "\n", "\\n")
}

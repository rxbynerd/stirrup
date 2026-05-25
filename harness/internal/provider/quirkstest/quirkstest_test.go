package quirkstest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
)

// TestFixturesScrubbed is the CI gate that enforces design risk 4:
// no fixture committed to the repository may carry an upstream
// credential or other sensitive substring that ScrubFixture would
// rewrite. The previous state shipped ScrubFixture with zero call
// sites, so a real wire capture committed with a Bearer token in it
// would have landed unnoticed.
//
// The test walks every file under
// harness/internal/provider/testdata/quirks/ and asserts ScrubFixture
// is a no-op against its content: scrub(bytes) == bytes. Any future
// fixture that carries a sensitive substring fails the build with a
// path-pinned message, naming the file the operator needs to revisit.
//
// Run from the quirkstest package (not the provider package) so the
// helper imports do not pull a real provider symbol into a test-only
// build. The fixture root is resolved relative to this test file,
// which sits at harness/internal/provider/quirkstest/; the fixtures
// live in a sibling subdirectory of the parent.
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
		// A zero-fixture state means either the fixture directory was
		// renamed (in which case the gate is dead) or the harness has
		// no fixtures yet (unlikely after Step 2). Fail loudly either
		// way so the CI gate cannot silently regress to a no-op.
		t.Fatalf("walked %s and found zero fixtures; the gate is no longer enforcing anything", fixtureRoot)
	}
}

// diff is a small helper for surfacing the first byte at which raw
// and scrubbed diverge, so the test error message points an operator
// at the substring rather than dumping a multi-line file.
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

package version

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVersion_DefaultsWithoutLdflags asserts sensible defaults when no -ldflags are passed.
//
// Mutates package-level vars; must NOT call t.Parallel.
func TestVersion_DefaultsWithoutLdflags(t *testing.T) {
	require.Equal(t, "dev", Version())
	require.Empty(t, Commit())
	require.Equal(t, "dev", Full())
}

// TestFull_FormatsVersionAndCommit exercises the formatting branches by temporarily mutating the
// package-level vars; t.Cleanup restores the originals.
func TestFull_FormatsVersionAndCommit(t *testing.T) {
	origV, origC := version, commit
	t.Cleanup(func() { version, commit = origV, origC })

	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{"version and commit", "v1.2.3", "ab74b75", "v1.2.3 (ab74b75)"},
		{"version only", "v1.2.3", "", "v1.2.3"},
		{"main branch", "main", "deadbeef", "main (deadbeef)"},
		{"dev default", "dev", "", "dev"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version, commit = tc.version, tc.commit
			require.Equal(t, tc.want, Full())
		})
	}
}

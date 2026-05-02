package version

import "testing"

// TestDefaults asserts the link-time-injected vars hold sensible defaults
// when no -ldflags are passed (i.e. local `go build` / `go test`).
//
// These tests mutate the package-level vars and therefore must NOT call
// t.Parallel.
func TestDefaults(t *testing.T) {
	if got := Version(); got != "dev" {
		t.Fatalf("Version() = %q, want %q", got, "dev")
	}
	if got := Commit(); got != "" {
		t.Fatalf("Commit() = %q, want empty string", got)
	}
	if got := Full(); got != "dev" {
		t.Fatalf("Full() = %q, want %q", got, "dev")
	}
}

// TestFull exercises the formatting branches by temporarily mutating the
// package-level vars. t.Cleanup restores the originals so subsequent tests
// (and parallel test binaries) see the unmodified defaults.
func TestFull(t *testing.T) {
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
			if got := Full(); got != tc.want {
				t.Fatalf("Full() = %q, want %q", got, tc.want)
			}
		})
	}
}

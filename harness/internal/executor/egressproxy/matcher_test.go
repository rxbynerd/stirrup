package egressproxy

import (
	"strings"
	"testing"
)

func TestNewMatcher_ParsesEntries(t *testing.T) {
	m, err := NewMatcher([]string{
		"example.com",
		"*.example.com",
		"api.github.com:443",
		"apt.example.org:80",
		"  EXAMPLE.NET.  ",
		"",
	})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	got := m.Entries()
	want := map[string]bool{
		"example.com:443":     true,
		"*.example.com:443":   true,
		"api.github.com:443":  true,
		"apt.example.org:80":  true,
		"example.net:443":     true,
	}

	if len(got) != len(want) {
		t.Fatalf("entries: got %d, want %d (%v)", len(got), len(want), got)
	}
	for _, entry := range got {
		if !want[entry] {
			t.Errorf("unexpected entry %q", entry)
		}
	}
}

func TestNewMatcher_RejectsMalformed(t *testing.T) {
	cases := []string{
		"example.com:abc",        // non-numeric port
		"example.com:0",          // out of range
		"example.com:99999",      // out of range
		"foo bar.com",            // whitespace inside host
		":443",                   // empty host
		"*.:443",                 // empty host with wildcard
	}
	for _, c := range cases {
		if _, err := NewMatcher([]string{c}); err == nil {
			t.Errorf("expected error for malformed entry %q", c)
		}
	}
}

func TestMatcher_ExactMatch(t *testing.T) {
	m, err := NewMatcher([]string{"example.com"})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	cases := []struct {
		host string
		port int
		want bool
	}{
		{"example.com", 443, true},
		{"EXAMPLE.COM", 443, true},
		{"example.com.", 443, true}, // trailing dot canonicalisation
		{"example.com", 80, false},  // port mismatch
		{"foo.example.com", 443, false},
		{"evilexample.com", 443, false},
		{"", 443, false},
	}
	for _, tc := range cases {
		got := m.Match(tc.host, tc.port)
		if got != tc.want {
			t.Errorf("Match(%q,%d): got %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}
}

func TestMatcher_Wildcard(t *testing.T) {
	m, err := NewMatcher([]string{"*.example.com"})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	cases := []struct {
		host string
		port int
		want bool
	}{
		{"a.example.com", 443, true},
		{"a.b.example.com", 443, true},
		{"deeply.nested.sub.example.com", 443, true},
		// Wildcard does NOT match the bare apex per RFC 6125.
		{"example.com", 443, false},
		// A host that merely ends with `example.com` but lacks the `.` boundary
		// must not match (defeat `evilexample.com` style attacks).
		{"evilexample.com", 443, false},
		// Port mismatch
		{"a.example.com", 80, false},
	}
	for _, tc := range cases {
		got := m.Match(tc.host, tc.port)
		if got != tc.want {
			t.Errorf("Match(%q,%d): got %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}
}

func TestMatcher_PortSuffixIsExact(t *testing.T) {
	m, err := NewMatcher([]string{"apt.example.org:80"})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	if !m.Match("apt.example.org", 80) {
		t.Error("expected port-80 match")
	}
	// A port-suffixed entry must NOT also allow 443.
	if m.Match("apt.example.org", 443) {
		t.Error("port-80 entry must not also match port 443")
	}
}

func TestMatcher_DefaultPortIs443(t *testing.T) {
	m, err := NewMatcher([]string{"example.com"})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	if !m.Match("example.com", 443) {
		t.Error("entry without explicit port should default to 443")
	}
	for _, p := range []int{80, 8080, 22, 25} {
		if m.Match("example.com", p) {
			t.Errorf("entry without explicit port must not match port %d", p)
		}
	}
}

func TestMatcher_NilSafe(t *testing.T) {
	var m *Matcher
	if m.Match("example.com", 443) {
		t.Error("nil matcher should reject everything")
	}
	if entries := m.Entries(); entries != nil {
		t.Errorf("nil matcher Entries should be nil, got %v", entries)
	}
}

func TestMatcher_MixedEntries(t *testing.T) {
	m, err := NewMatcher([]string{
		"api.github.com",
		"*.githubusercontent.com",
		"registry.npmjs.org",
		"apt.example.org:80",
	})
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	allow := []struct {
		host string
		port int
	}{
		{"api.github.com", 443},
		{"raw.githubusercontent.com", 443},
		{"objects.githubusercontent.com", 443},
		{"registry.npmjs.org", 443},
		{"apt.example.org", 80},
	}
	for _, c := range allow {
		if !m.Match(c.host, c.port) {
			t.Errorf("expected allow: %s:%d", c.host, c.port)
		}
	}

	deny := []struct {
		host string
		port int
	}{
		{"github.com", 443},          // not allowlisted apex
		{"api.github.com", 80},       // wrong port
		{"githubusercontent.com", 443}, // wildcard apex not allowed
		{"npmjs.org", 443},
		{"apt.example.org", 443},     // port-80 entry must not allow 443
	}
	for _, c := range deny {
		if m.Match(c.host, c.port) {
			t.Errorf("expected deny: %s:%d", c.host, c.port)
		}
	}
}

func TestNewMatcher_ErrorMentionsEntry(t *testing.T) {
	_, err := NewMatcher([]string{"example.com:abc"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "example.com:abc") {
		t.Errorf("error should reference the offending entry, got: %v", err)
	}
}

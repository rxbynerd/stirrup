package egressproxy

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMatcher_ParsesEntries(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	m, err := NewMatcher([]string{
		"example.com",
		"*.example.com",
		"api.github.com:443",
		"apt.example.org:80",
		"  EXAMPLE.NET.  ",
		"",
	})
	must.NoError(err)

	got := m.Entries()
	want := map[string]bool{
		"example.com:443":    true,
		"*.example.com:443":  true,
		"api.github.com:443": true,
		"apt.example.org:80": true,
		"example.net:443":    true,
	}

	must.Len(got, len(want), "entries: %v", got)
	for _, entry := range got {
		check.True(want[entry], "unexpected entry %q", entry)
	}
}

func TestNewMatcher_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"non-numeric port", "example.com:abc"},
		{"port zero", "example.com:0"},
		{"port above 65535", "example.com:99999"},
		{"whitespace inside host", "foo bar.com"},
		{"empty host", ":443"},
		{"empty host with wildcard", "*.:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := NewMatcher([]string{tc.entry})
			must.Error(err, "entry %q", tc.entry)
		})
	}
}

func TestMatcher_ExactMatch(t *testing.T) {
	must := require.New(t)
	m, err := NewMatcher([]string{"example.com"})
	must.NoError(err)

	cases := []struct {
		name string
		host string
		port int
		want bool
	}{
		{"exact host and port", "example.com", 443, true},
		{"host is case-insensitive", "EXAMPLE.COM", 443, true},
		{"trailing dot is canonicalised", "example.com.", 443, true},
		{"port mismatch", "example.com", 80, false},
		{"subdomain does not match", "foo.example.com", 443, false},
		{"suffix without label boundary", "evilexample.com", 443, false},
		{"empty host", "", 443, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := m.Match(tc.host, tc.port)
			must.Equal(tc.want, got)
		})
	}
}

func TestMatcher_Wildcard(t *testing.T) {
	must := require.New(t)
	m, err := NewMatcher([]string{"*.example.com"})
	must.NoError(err)

	cases := []struct {
		name string
		host string
		port int
		want bool
	}{
		{"single-label subdomain", "a.example.com", 443, true},
		{"multi-label subdomain", "a.b.example.com", 443, true},
		{"deeply nested subdomain", "deeply.nested.sub.example.com", 443, true},
		// The wildcard does not match the bare apex (RFC 6125).
		{"bare apex", "example.com", 443, false},
		// The wildcard requires a label boundary, so suffix lookalikes are rejected.
		{"suffix without label boundary", "evilexample.com", 443, false},
		{"port mismatch", "a.example.com", 80, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := m.Match(tc.host, tc.port)
			must.Equal(tc.want, got)
		})
	}
}

func TestMatcher_PortSuffixIsExact(t *testing.T) {
	must := require.New(t)
	m, err := NewMatcher([]string{"apt.example.org:80"})
	must.NoError(err)

	must.True(m.Match("apt.example.org", 80))
	// A port-suffixed entry must not also allow 443.
	must.False(m.Match("apt.example.org", 443))
}

func TestMatcher_DefaultPortIs443(t *testing.T) {
	must := require.New(t)
	m, err := NewMatcher([]string{"example.com"})
	must.NoError(err)

	must.True(m.Match("example.com", 443), "entry without explicit port should default to 443")
	for _, port := range []int{80, 8080, 22, 25} {
		t.Run(fmt.Sprintf("port %d", port), func(t *testing.T) {
			must := require.New(t)
			must.False(m.Match("example.com", port), "entry without explicit port must not match")
		})
	}
}

func TestMatcher_NilReceiverIsSafe(t *testing.T) {
	must := require.New(t)
	var m *Matcher
	must.False(m.Match("example.com", 443), "nil matcher should reject everything")
	must.Nil(m.Entries())
}

func TestMatcher_MixedEntries(t *testing.T) {
	must := require.New(t)
	m, err := NewMatcher([]string{
		"api.github.com",
		"*.githubusercontent.com",
		"registry.npmjs.org",
		"apt.example.org:80",
	})
	must.NoError(err)

	cases := []struct {
		name string
		host string
		port int
		want bool
	}{
		{"exact host is allowed", "api.github.com", 443, true},
		{"wildcard subdomain is allowed", "raw.githubusercontent.com", 443, true},
		{"second wildcard subdomain is allowed", "objects.githubusercontent.com", 443, true},
		{"registry host is allowed", "registry.npmjs.org", 443, true},
		{"port-suffixed host is allowed on its port", "apt.example.org", 80, true},
		{"apex of an allowlisted host is denied", "github.com", 443, false},
		{"allowlisted host on another port is denied", "api.github.com", 80, false},
		{"wildcard apex is denied", "githubusercontent.com", 443, false},
		{"unlisted host is denied", "npmjs.org", 443, false},
		{"port-80 entry does not allow 443", "apt.example.org", 443, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := m.Match(tc.host, tc.port)
			must.Equal(tc.want, got)
		})
	}
}

func TestNewMatcher_ErrorMentionsEntry(t *testing.T) {
	must := require.New(t)
	_, err := NewMatcher([]string{"example.com:abc"})
	must.Error(err)
	must.ErrorContains(err, "example.com:abc", "error should reference the offending entry")
}

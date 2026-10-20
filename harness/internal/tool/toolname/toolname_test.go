package toolname

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyFor_KnownProviders(t *testing.T) {
	cases := []struct {
		name                  string
		providerType          string
		wantMaxLen            int
		wantAllowHyphen       bool
		wantAllowLeadingDigit bool
	}{
		{"anthropic", "anthropic", 64, true, true},
		{"openai-compatible", "openai-compatible", 64, true, true},
		{"openai-responses", "openai-responses", 64, true, true},
		{"bedrock", "bedrock", 64, true, true},
		{"gemini", "gemini", 64, false, false},
		// Unknown providers fall through to the strictest policy.
		{"unknown provider", "made-up-provider", 64, false, false},
		{"empty provider type", "", 64, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			p := PolicyFor(tc.providerType)
			must.Equal(tc.wantMaxLen, p.MaxLen)
			must.Equal(tc.wantAllowHyphen, p.AllowHyphen)
			must.Equal(tc.wantAllowLeadingDigit, p.AllowLeadingDigit)
		})
	}
}

func TestSanitize_PunctuationAndSpaces(t *testing.T) {
	must := require.New(t)
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
	got := sanitize("mcp_server.tool name/with stuff", p)
	must.Equal("mcp_server_tool_name_with_stuff", got)
}

func TestSanitize_HyphenAllowedAndDisallowed(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		must := require.New(t)
		p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
		got := sanitize("mcp-server-tool", p)
		must.Equal("mcp-server-tool", got)
	})
	t.Run("disallowed", func(t *testing.T) {
		must := require.New(t)
		p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
		got := sanitize("mcp-server-tool", p)
		must.Equal("mcp_server_tool", got)
	})
}

func TestSanitize_LeadingDigitUnderscorePrepend(t *testing.T) {
	must := require.New(t)
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: false}
	got := sanitize("123abc", p)
	must.Equal("_123abc", got)
}

func TestSanitize_LeadingDigitWhenAllowed(t *testing.T) {
	must := require.New(t)
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
	got := sanitize("123abc", p)
	must.Equal("123abc", got)
}

func TestSanitize_NonASCIIReplaced(t *testing.T) {
	must := require.New(t)
	p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("café_tool", p)
	// "é" is a single non-ASCII rune; sanitize replaces it with one
	// underscore regardless of its byte width (the range loop yields
	// runes, not bytes).
	must.True(strings.HasPrefix(got, "caf") && strings.HasSuffix(got, "_tool"), "got %q", got)
}

func TestSanitize_TruncatesAtMaxLen(t *testing.T) {
	must := require.New(t)
	p := Policy{MaxLen: 16, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("a_very_long_tool_name_well_over_sixteen_chars", p)
	must.Len(got, 16, "got %q", got)
}

func TestSanitize_EmptyNamePlaceholder(t *testing.T) {
	must := require.New(t)
	p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("", p)
	must.Equal("_unnamed", got)
}

func TestBuild_RoundTripPreservesIdentity(t *testing.T) {
	must := require.New(t)
	names := []string{"read_file", "list_directory", "grep_files", "find_files"}
	m, err := Build(names, PolicyFor("openai-compatible"))
	must.NoError(err)
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			must := require.New(t)
			ext := m.Translate(n)
			must.Equal(n, ext)
			must.Equal(n, m.Reverse(ext))
		})
	}
}

func TestBuild_MCPNamesNormalizedForGemini(t *testing.T) {
	must := require.New(t)
	names := []string{
		"mcp_jira_create-issue",
		"mcp_jira_search.tickets",
		"mcp_slack_post message",
	}
	m, err := Build(names, PolicyFor("gemini"))
	must.NoError(err)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			must := require.New(t)
			// Gemini policy forbids hyphens and disallows whitespace and
			// punctuation outside [a-zA-Z0-9_].
			ext := m.Translate(n)
			must.False(strings.ContainsAny(ext, "-. "), "Translate(%q) = %q still contains a disallowed character", n, ext)
			must.Equal(n, m.Reverse(ext))
		})
	}
}

func TestBuild_CollisionAfterNormalizationIsResolvedDeterministically(t *testing.T) {
	must := require.New(t)
	// Both names normalise to "mcp_jira_issue" under the gemini policy
	// (different punctuation but identical sanitized form). The
	// collision must be resolved via a hash suffix derived from the
	// internal name so the resolution is stable.
	names := []string{"mcp_jira-issue", "mcp_jira.issue"}
	policy := PolicyFor("gemini")

	m1, err := Build(names, policy)
	must.NoError(err)
	m2, err := Build(names, policy)
	must.NoError(err)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			must := require.New(t)
			must.Equal(m1.Translate(n), m2.Translate(n))
			must.Equal(n, m1.Reverse(m1.Translate(n)))
		})
	}

	must.NotEqual(m1.Translate(names[0]), m1.Translate(names[1]),
		"collision not resolved: both names normalised to %q", m1.Translate(names[0]))
}

func TestBuild_DuplicateInternalNameRejected(t *testing.T) {
	must := require.New(t)
	names := []string{"read_file", "read_file"}
	_, err := Build(names, PolicyFor("anthropic"))
	must.Error(err)
}

func TestBuild_IrresolvableCollisionErrors(t *testing.T) {
	must := require.New(t)
	// Three distinct names sanitize to the same single character under
	// MaxLen=1, so the collision cannot be resolved. It must surface as an
	// error rather than alias a tool call to the wrong handler.
	names := []string{"aa", "ab", "ac"}
	policy := Policy{MaxLen: 1, AllowHyphen: false, AllowLeadingDigit: true}
	_, err := Build(names, policy)
	must.ErrorContains(err, "cannot resolve collision")
}

func TestBuild_PathologicalMaxLenResolvesColludingPair(t *testing.T) {
	must := require.New(t)
	// Under MaxLen=2 the 7-char hash suffix does not fit, so disambiguation
	// falls back to a shorter suffix that still keeps the pair distinct.
	names := []string{"jira_alpha", "jira_bravo"}
	policy := Policy{MaxLen: 2, AllowHyphen: false, AllowLeadingDigit: true}
	m, err := Build(names, policy)
	must.NoError(err)

	a, b := m.Translate(names[0]), m.Translate(names[1])
	must.NotEqual(a, b, "collision not resolved: both names normalised to %q", a)
	must.LessOrEqual(len(a), policy.MaxLen, "external name %q", a)
	must.LessOrEqual(len(b), policy.MaxLen, "external name %q", b)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			must := require.New(t)
			must.Equal(n, m.Reverse(m.Translate(n)))
		})
	}
}

func TestBuild_LongNamesGetHashSuffixWhenColliding(t *testing.T) {
	must := require.New(t)
	// Two long names that share a 64-char prefix would collide after
	// hard truncation; the hash suffix must keep them distinct.
	base := strings.Repeat("a", 60) + "_x"
	names := []string{base + "_one", base + "_two"}
	policy := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	m, err := Build(names, policy)
	must.NoError(err)

	a, b := m.Translate(names[0]), m.Translate(names[1])
	must.NotEqual(a, b, "collision not resolved: both names normalised to %q", a)
	must.LessOrEqual(len(a), policy.MaxLen, "external name %q", a)
	must.LessOrEqual(len(b), policy.MaxLen, "external name %q", b)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			must := require.New(t)
			must.Equal(n, m.Reverse(m.Translate(n)))
		})
	}
}

func TestBuild_DeterministicAcrossOrderings(t *testing.T) {
	must := require.New(t)
	forward := []string{"mcp_jira-issue", "mcp_jira.issue", "read_file"}
	reversed := []string{"read_file", "mcp_jira.issue", "mcp_jira-issue"}

	mForward, err := BuildSorted(forward, PolicyFor("gemini"))
	must.NoError(err)
	mReversed, err := BuildSorted(reversed, PolicyFor("gemini"))
	must.NoError(err)

	for _, n := range forward {
		t.Run(n, func(t *testing.T) {
			must := require.New(t)
			must.Equal(mForward.Translate(n), mReversed.Translate(n))
		})
	}
}

func TestMapping_NilSafePassThrough(t *testing.T) {
	must := require.New(t)
	var m *Mapping
	must.Equal("anything", m.Translate("anything"))
	must.Equal("anything", m.Reverse("anything"))
}

func TestMapping_MissingKeyPassThrough(t *testing.T) {
	must := require.New(t)
	m, err := Build([]string{"a", "b"}, PolicyFor("anthropic"))
	must.NoError(err)
	must.Equal("not_registered", m.Translate("not_registered"))
	must.Equal("not_registered", m.Reverse("not_registered"))
}

// BuildFromCandidates backs the toolset-profile presenter's alias resolution.
// Distinct keys with identical candidates are disambiguated, not rejected.
func TestBuildFromCandidates_IdenticalCandidatesDisambiguated(t *testing.T) {
	must := require.New(t)
	keys := []string{"grep_files", "find_files"}
	candidates := []string{"search", "search"}
	policy := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}

	m, err := BuildFromCandidates(keys, candidates, policy)
	must.NoError(err)

	gf := m.Translate("grep_files")
	ff := m.Translate("find_files")
	must.NotEqual(gf, ff)

	// Exactly one keeps the bare candidate; the other carries the
	// "_<6hex>" suffix derived from its key.
	bareCount := 0
	for _, ext := range []string{gf, ff} {
		if ext == "search" {
			bareCount++
		}
	}
	must.Equal(1, bareCount, "gf=%q ff=%q", gf, ff)

	must.Equal("grep_files", m.Reverse(gf))
	must.Equal("find_files", m.Reverse(ff))
}

// A duplicate key is a caller bug and is rejected so registry uniqueness holds.
func TestBuildFromCandidates_DuplicateKeyRejected(t *testing.T) {
	must := require.New(t)
	_, err := BuildFromCandidates(
		[]string{"dup", "dup"},
		[]string{"a", "b"},
		Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true},
	)
	must.Error(err)
}

func TestBuildFromCandidates_LengthMismatchRejected(t *testing.T) {
	must := require.New(t)
	_, err := BuildFromCandidates(
		[]string{"a", "b"},
		[]string{"x"},
		Policy{MaxLen: 64},
	)
	must.Error(err)
}

func TestBuild_ResolvesSanitizeCollisions(t *testing.T) {
	must := require.New(t)
	names := []string{"mcp_jira-issue", "mcp_jira.issue"}
	m, err := Build(names, PolicyFor("gemini"))
	must.NoError(err)
	must.NotEqual(m.Translate(names[0]), m.Translate(names[1]))
}

func TestBuild_AcceptsAllProviderNamesForCommonRegistry(t *testing.T) {
	// A realistic registry normalises cleanly under every provider's policy.
	names := []string{
		"read_file", "list_directory", "grep_files", "find_files",
		"write_file", "edit_file", "run_command", "web_fetch",
		"spawn_agent",
		"mcp_jira_create-issue", "mcp_slack_post.message",
	}
	for _, p := range []string{"anthropic", "openai-compatible", "openai-responses", "bedrock", "gemini"} {
		t.Run(p, func(t *testing.T) {
			must := require.New(t)
			m, err := Build(names, PolicyFor(p))
			must.NoError(err)
			for _, n := range names {
				t.Run(n, func(t *testing.T) {
					must := require.New(t)
					ext := m.Translate(n)
					must.NotEmpty(ext)
					must.Equal(n, m.Reverse(ext))
				})
			}
		})
	}
}

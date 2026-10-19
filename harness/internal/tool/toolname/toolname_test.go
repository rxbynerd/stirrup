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
			p := PolicyFor(tc.providerType)
			require.Equal(t, tc.wantMaxLen, p.MaxLen)
			require.Equal(t, tc.wantAllowHyphen, p.AllowHyphen)
			require.Equal(t, tc.wantAllowLeadingDigit, p.AllowLeadingDigit)
		})
	}
}

func TestSanitize_PunctuationAndSpaces(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
	got := sanitize("mcp_server.tool name/with stuff", p)
	require.Equal(t, "mcp_server_tool_name_with_stuff", got)
}

func TestSanitize_HyphenAllowedAndDisallowed(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
		got := sanitize("mcp-server-tool", p)
		require.Equal(t, "mcp-server-tool", got)
	})
	t.Run("disallowed", func(t *testing.T) {
		p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
		got := sanitize("mcp-server-tool", p)
		require.Equal(t, "mcp_server_tool", got)
	})
}

func TestSanitize_LeadingDigitUnderscorePrepend(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: false}
	got := sanitize("123abc", p)
	require.Equal(t, "_123abc", got)
}

func TestSanitize_LeadingDigitWhenAllowed(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
	got := sanitize("123abc", p)
	require.Equal(t, "123abc", got)
}

func TestSanitize_NonASCIIReplaced(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("café_tool", p)
	// "é" is a single non-ASCII rune; sanitize replaces it with one
	// underscore regardless of its byte width (the range loop yields
	// runes, not bytes).
	require.True(t, strings.HasPrefix(got, "caf") && strings.HasSuffix(got, "_tool"), "got %q", got)
}

func TestSanitize_TruncatesAtMaxLen(t *testing.T) {
	p := Policy{MaxLen: 16, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("a_very_long_tool_name_well_over_sixteen_chars", p)
	require.Len(t, got, 16, "got %q", got)
}

func TestSanitize_EmptyNamePlaceholder(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("", p)
	require.Equal(t, "_unnamed", got)
}

func TestBuild_RoundTripPreservesIdentity(t *testing.T) {
	names := []string{"read_file", "list_directory", "grep_files", "find_files"}
	m, err := Build(names, PolicyFor("openai-compatible"))
	require.NoError(t, err)
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			ext := m.Translate(n)
			require.Equal(t, n, ext)
			require.Equal(t, n, m.Reverse(ext))
		})
	}
}

func TestBuild_MCPNamesNormalizedForGemini(t *testing.T) {
	names := []string{
		"mcp_jira_create-issue",
		"mcp_jira_search.tickets",
		"mcp_slack_post message",
	}
	m, err := Build(names, PolicyFor("gemini"))
	require.NoError(t, err)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			// Gemini policy forbids hyphens and disallows whitespace and
			// punctuation outside [a-zA-Z0-9_].
			ext := m.Translate(n)
			require.False(t, strings.ContainsAny(ext, "-. "), "Translate(%q) = %q still contains a disallowed character", n, ext)
			require.Equal(t, n, m.Reverse(ext))
		})
	}
}

func TestBuild_CollisionAfterNormalizationIsResolvedDeterministically(t *testing.T) {
	// Both names normalise to "mcp_jira_issue" under the gemini policy
	// (different punctuation but identical sanitized form). The
	// collision must be resolved via a hash suffix derived from the
	// internal name so the resolution is stable.
	names := []string{"mcp_jira-issue", "mcp_jira.issue"}
	policy := PolicyFor("gemini")

	m1, err := Build(names, policy)
	require.NoError(t, err)
	m2, err := Build(names, policy)
	require.NoError(t, err)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			require.Equal(t, m1.Translate(n), m2.Translate(n))
			require.Equal(t, n, m1.Reverse(m1.Translate(n)))
		})
	}

	require.NotEqual(t, m1.Translate(names[0]), m1.Translate(names[1]),
		"collision not resolved: both names normalised to %q", m1.Translate(names[0]))
}

func TestBuild_DuplicateInternalNameRejected(t *testing.T) {
	names := []string{"read_file", "read_file"}
	_, err := Build(names, PolicyFor("anthropic"))
	require.Error(t, err)
}

func TestBuild_IrresolvableCollisionErrors(t *testing.T) {
	// Three distinct names sanitize to the same single character under
	// MaxLen=1, so the collision cannot be resolved. It must surface as an
	// error rather than alias a tool call to the wrong handler.
	names := []string{"aa", "ab", "ac"}
	policy := Policy{MaxLen: 1, AllowHyphen: false, AllowLeadingDigit: true}
	_, err := Build(names, policy)
	require.ErrorContains(t, err, "cannot resolve collision")
}

func TestBuild_PathologicalMaxLenResolvesColludingPair(t *testing.T) {
	// Under MaxLen=2 the 7-char hash suffix does not fit, so disambiguation
	// falls back to a shorter suffix that still keeps the pair distinct.
	names := []string{"jira_alpha", "jira_bravo"}
	policy := Policy{MaxLen: 2, AllowHyphen: false, AllowLeadingDigit: true}
	m, err := Build(names, policy)
	require.NoError(t, err)

	a, b := m.Translate(names[0]), m.Translate(names[1])
	require.NotEqual(t, a, b, "collision not resolved: both names normalised to %q", a)
	require.LessOrEqual(t, len(a), policy.MaxLen, "external name %q", a)
	require.LessOrEqual(t, len(b), policy.MaxLen, "external name %q", b)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			require.Equal(t, n, m.Reverse(m.Translate(n)))
		})
	}
}

func TestBuild_LongNamesGetHashSuffixWhenColliding(t *testing.T) {
	// Two long names that share a 64-char prefix would collide after
	// hard truncation; the hash suffix must keep them distinct.
	base := strings.Repeat("a", 60) + "_x"
	names := []string{base + "_one", base + "_two"}
	policy := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	m, err := Build(names, policy)
	require.NoError(t, err)

	a, b := m.Translate(names[0]), m.Translate(names[1])
	require.NotEqual(t, a, b, "collision not resolved: both names normalised to %q", a)
	require.LessOrEqual(t, len(a), policy.MaxLen, "external name %q", a)
	require.LessOrEqual(t, len(b), policy.MaxLen, "external name %q", b)

	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			require.Equal(t, n, m.Reverse(m.Translate(n)))
		})
	}
}

func TestBuild_DeterministicAcrossOrderings(t *testing.T) {
	forward := []string{"mcp_jira-issue", "mcp_jira.issue", "read_file"}
	reversed := []string{"read_file", "mcp_jira.issue", "mcp_jira-issue"}

	mForward, err := BuildSorted(forward, PolicyFor("gemini"))
	require.NoError(t, err)
	mReversed, err := BuildSorted(reversed, PolicyFor("gemini"))
	require.NoError(t, err)

	for _, n := range forward {
		t.Run(n, func(t *testing.T) {
			require.Equal(t, mForward.Translate(n), mReversed.Translate(n))
		})
	}
}

func TestMapping_NilSafePassThrough(t *testing.T) {
	var m *Mapping
	require.Equal(t, "anything", m.Translate("anything"))
	require.Equal(t, "anything", m.Reverse("anything"))
}

func TestMapping_MissingKeyPassThrough(t *testing.T) {
	m, err := Build([]string{"a", "b"}, PolicyFor("anthropic"))
	require.NoError(t, err)
	require.Equal(t, "not_registered", m.Translate("not_registered"))
	require.Equal(t, "not_registered", m.Reverse("not_registered"))
}

// BuildFromCandidates backs the toolset-profile presenter's alias resolution.
// Distinct keys with identical candidates are disambiguated, not rejected.
func TestBuildFromCandidates_IdenticalCandidatesDisambiguated(t *testing.T) {
	keys := []string{"grep_files", "find_files"}
	candidates := []string{"search", "search"}
	policy := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}

	m, err := BuildFromCandidates(keys, candidates, policy)
	require.NoError(t, err)

	gf := m.Translate("grep_files")
	ff := m.Translate("find_files")
	require.NotEqual(t, gf, ff)

	// Exactly one keeps the bare candidate; the other carries the
	// "_<6hex>" suffix derived from its key.
	bareCount := 0
	for _, ext := range []string{gf, ff} {
		if ext == "search" {
			bareCount++
		}
	}
	require.Equal(t, 1, bareCount, "gf=%q ff=%q", gf, ff)

	require.Equal(t, "grep_files", m.Reverse(gf))
	require.Equal(t, "find_files", m.Reverse(ff))
}

// A duplicate key is a caller bug and is rejected so registry uniqueness holds.
func TestBuildFromCandidates_DuplicateKeyRejected(t *testing.T) {
	_, err := BuildFromCandidates(
		[]string{"dup", "dup"},
		[]string{"a", "b"},
		Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true},
	)
	require.Error(t, err)
}

func TestBuildFromCandidates_LengthMismatchRejected(t *testing.T) {
	_, err := BuildFromCandidates(
		[]string{"a", "b"},
		[]string{"x"},
		Policy{MaxLen: 64},
	)
	require.Error(t, err)
}

func TestBuild_ResolvesSanitizeCollisions(t *testing.T) {
	names := []string{"mcp_jira-issue", "mcp_jira.issue"}
	m, err := Build(names, PolicyFor("gemini"))
	require.NoError(t, err)
	require.NotEqual(t, m.Translate(names[0]), m.Translate(names[1]))
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
			m, err := Build(names, PolicyFor(p))
			require.NoError(t, err)
			for _, n := range names {
				t.Run(n, func(t *testing.T) {
					ext := m.Translate(n)
					require.NotEmpty(t, ext)
					require.Equal(t, n, m.Reverse(ext))
				})
			}
		})
	}
}

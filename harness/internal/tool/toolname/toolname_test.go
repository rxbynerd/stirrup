package toolname

import (
	"strings"
	"testing"
)

func TestPolicyFor_KnownProviders(t *testing.T) {
	cases := []struct {
		providerType      string
		wantMaxLen        int
		wantAllowHyphen   bool
		wantAllowLeading0 bool
	}{
		{"anthropic", 64, true, true},
		{"openai-compatible", 64, true, true},
		{"openai-responses", 64, true, true},
		{"bedrock", 64, true, true},
		{"gemini", 64, false, false},
		// Unknown providers fall through to the strictest policy.
		{"made-up-provider", 64, false, false},
		{"", 64, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.providerType, func(t *testing.T) {
			p := PolicyFor(tc.providerType)
			if p.MaxLen != tc.wantMaxLen {
				t.Errorf("MaxLen = %d, want %d", p.MaxLen, tc.wantMaxLen)
			}
			if p.AllowHyphen != tc.wantAllowHyphen {
				t.Errorf("AllowHyphen = %v, want %v", p.AllowHyphen, tc.wantAllowHyphen)
			}
			if p.AllowLeadingDigit != tc.wantAllowLeading0 {
				t.Errorf("AllowLeadingDigit = %v, want %v", p.AllowLeadingDigit, tc.wantAllowLeading0)
			}
		})
	}
}

func TestSanitize_PunctuationAndSpaces(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
	got := sanitize("mcp_server.tool name/with stuff", p)
	want := "mcp_server_tool_name_with_stuff"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitize_HyphenAllowedAndDisallowed(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
		got := sanitize("mcp-server-tool", p)
		if got != "mcp-server-tool" {
			t.Errorf("got %q, want hyphens preserved", got)
		}
	})
	t.Run("disallowed", func(t *testing.T) {
		p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
		got := sanitize("mcp-server-tool", p)
		if got != "mcp_server_tool" {
			t.Errorf("got %q, want hyphens substituted", got)
		}
	})
}

func TestSanitize_LeadingDigitUnderscorePrepend(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: false}
	got := sanitize("123abc", p)
	if got != "_123abc" {
		t.Errorf("got %q, want underscore prepended", got)
	}
}

func TestSanitize_LeadingDigitWhenAllowed(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: false, AllowLeadingDigit: true}
	got := sanitize("123abc", p)
	if got != "123abc" {
		t.Errorf("got %q, want digits preserved verbatim", got)
	}
}

func TestSanitize_NonASCIIReplaced(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("café_tool", p)
	// "é" is two bytes in UTF-8; sanitize replaces each invalid byte
	// with one underscore, so multi-byte runes flatten to underscores.
	if !strings.HasPrefix(got, "caf") || !strings.HasSuffix(got, "_tool") {
		t.Errorf("got %q, want non-ASCII rune substituted", got)
	}
}

func TestSanitize_TruncatesAtMaxLen(t *testing.T) {
	p := Policy{MaxLen: 16, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("a_very_long_tool_name_well_over_sixteen_chars", p)
	if len(got) != 16 {
		t.Errorf("got %q (len=%d), want length 16", got, len(got))
	}
}

func TestSanitize_EmptyNamePlaceholder(t *testing.T) {
	p := Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}
	got := sanitize("", p)
	if got != "_unnamed" {
		t.Errorf("got %q, want placeholder", got)
	}
}

func TestBuild_RoundTripPreservesIdentity(t *testing.T) {
	names := []string{"read_file", "list_directory", "search_files"}
	m, err := Build(names, PolicyFor("openai-compatible"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, n := range names {
		ext := m.Translate(n)
		if ext != n {
			t.Errorf("Translate(%q) = %q, want unchanged", n, ext)
		}
		if got := m.Reverse(ext); got != n {
			t.Errorf("Reverse(%q) = %q, want %q", ext, got, n)
		}
	}
}

func TestBuild_MCPNamesNormalizedForGemini(t *testing.T) {
	names := []string{
		"mcp_jira_create-issue",
		"mcp_jira_search.tickets",
		"mcp_slack_post message",
	}
	m, err := Build(names, PolicyFor("gemini"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, n := range names {
		ext := m.Translate(n)
		// Gemini policy forbids hyphens and disallows whitespace and
		// punctuation outside [a-zA-Z0-9_].
		if strings.ContainsAny(ext, "-. ") {
			t.Errorf("Translate(%q) = %q still contains a disallowed character", n, ext)
		}
		// Round-trip must restore the original internal name.
		if got := m.Reverse(ext); got != n {
			t.Errorf("Reverse(%q) = %q, want %q", ext, got, n)
		}
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
	if err != nil {
		t.Fatalf("Build(first): %v", err)
	}
	m2, err := Build(names, policy)
	if err != nil {
		t.Fatalf("Build(second): %v", err)
	}

	for _, n := range names {
		if m1.Translate(n) != m2.Translate(n) {
			t.Errorf("non-deterministic translation for %q: %q vs %q",
				n, m1.Translate(n), m2.Translate(n))
		}
		if got := m1.Reverse(m1.Translate(n)); got != n {
			t.Errorf("round-trip failed for %q: got %q", n, got)
		}
	}

	// The two external names must differ; otherwise the collision was
	// not resolved.
	if m1.Translate(names[0]) == m1.Translate(names[1]) {
		t.Fatalf("collision not resolved: both names normalised to %q",
			m1.Translate(names[0]))
	}
}

func TestBuild_DuplicateInternalNameRejected(t *testing.T) {
	names := []string{"read_file", "read_file"}
	if _, err := Build(names, PolicyFor("anthropic")); err == nil {
		t.Fatal("expected error for duplicate internal name, got nil")
	}
}

func TestBuild_IrresolvableCollisionErrors(t *testing.T) {
	// Force the stillCollides branch in Build: three distinct internal
	// names that all sanitize to the same single-character form under
	// MaxLen=1. The first wins the bare name; the second's
	// disambiguation suffix is the pathological-budget branch in
	// disambiguate (budget = MaxLen - len(suffix) < 1), which falls
	// back to a one-character truncated suffix. The third name's
	// disambiguation truncates to the same one character, so the
	// post-disambiguation external name collides with what the second
	// already claimed — the only legitimate way to reach the
	// stillCollides return at toolname.go:194.
	//
	// This is the spec's fail-closed guarantee: irresolvable collisions
	// must surface as an error before any wire request is issued, so a
	// silent alias cannot route a tool call to the wrong handler.
	names := []string{"aa", "ab", "ac"}
	policy := Policy{MaxLen: 1, AllowHyphen: false, AllowLeadingDigit: true}
	_, err := Build(names, policy)
	if err == nil {
		t.Fatal("expected error for irresolvable collision, got nil")
	}
	if !strings.Contains(err.Error(), "cannot resolve collision") {
		t.Errorf("expected stillCollides error wording, got: %v", err)
	}
}

func TestBuild_LongNamesGetHashSuffixWhenColliding(t *testing.T) {
	// Two long names that share a 64-char prefix would collide after
	// hard truncation; the hash suffix must keep them distinct.
	base := strings.Repeat("a", 60) + "_x"
	names := []string{base + "_one", base + "_two"}
	m, err := Build(names, Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a, b := m.Translate(names[0]), m.Translate(names[1])
	if a == b {
		t.Fatalf("collision not resolved: both names normalised to %q", a)
	}
	if len(a) > 64 || len(b) > 64 {
		t.Errorf("disambiguation exceeded MaxLen: %d, %d", len(a), len(b))
	}
	for _, n := range names {
		if got := m.Reverse(m.Translate(n)); got != n {
			t.Errorf("round-trip failed for %q: got %q", n, got)
		}
	}
}

func TestBuild_DeterministicAcrossOrderings(t *testing.T) {
	a := []string{"mcp_jira-issue", "mcp_jira.issue", "read_file"}
	b := []string{"read_file", "mcp_jira.issue", "mcp_jira-issue"}

	m1, err := BuildSorted(a, PolicyFor("gemini"))
	if err != nil {
		t.Fatalf("BuildSorted(a): %v", err)
	}
	m2, err := BuildSorted(b, PolicyFor("gemini"))
	if err != nil {
		t.Fatalf("BuildSorted(b): %v", err)
	}
	for _, n := range a {
		if m1.Translate(n) != m2.Translate(n) {
			t.Errorf("BuildSorted is order-sensitive for %q: %q vs %q",
				n, m1.Translate(n), m2.Translate(n))
		}
	}
}

func TestMapping_NilSafePassThrough(t *testing.T) {
	var m *Mapping
	if got := m.Translate("anything"); got != "anything" {
		t.Errorf("nil Mapping.Translate returned %q, want pass-through", got)
	}
	if got := m.Reverse("anything"); got != "anything" {
		t.Errorf("nil Mapping.Reverse returned %q, want pass-through", got)
	}
}

func TestMapping_MissingKeyPassThrough(t *testing.T) {
	m, err := Build([]string{"a", "b"}, PolicyFor("anthropic"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := m.Translate("not_registered"); got != "not_registered" {
		t.Errorf("Translate of missing key returned %q, want pass-through", got)
	}
	if got := m.Reverse("not_registered"); got != "not_registered" {
		t.Errorf("Reverse of missing key returned %q, want pass-through", got)
	}
}

func TestBuild_AcceptsAllProviderNamesForCommonRegistry(t *testing.T) {
	// Smoke test: a realistic registry should normalise cleanly under
	// every provider's policy.
	names := []string{
		"read_file", "list_directory", "search_files",
		"write_file", "edit_file", "run_command", "web_fetch",
		"spawn_agent",
		"mcp_jira_create-issue", "mcp_slack_post.message",
	}
	for _, p := range []string{"anthropic", "openai-compatible", "openai-responses", "bedrock", "gemini"} {
		t.Run(p, func(t *testing.T) {
			m, err := Build(names, PolicyFor(p))
			if err != nil {
				t.Fatalf("Build for %s: %v", p, err)
			}
			for _, n := range names {
				ext := m.Translate(n)
				if ext == "" {
					t.Errorf("%s: empty external name for %q", p, n)
				}
				if got := m.Reverse(ext); got != n {
					t.Errorf("%s: Reverse(%q) = %q, want %q", p, ext, got, n)
				}
			}
		})
	}
}

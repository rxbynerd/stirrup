package security

import "testing"

func TestScrub_AnthropicKey(t *testing.T) {
	input := "key is sk-ant-abc123_DEF-456"
	got := Scrub(input)
	want := "key is [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_GitHubPAT(t *testing.T) {
	input := "token: ghp_abc123XYZ"
	got := Scrub(input)
	want := "token: [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_GitHubAppToken(t *testing.T) {
	input := "ghs_tokenValue123"
	got := Scrub(input)
	want := "[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_AWSAccessKey(t *testing.T) {
	input := "AKIAIOSFODNN7EXAMPLE"
	got := Scrub(input)
	want := "[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_BearerToken(t *testing.T) {
	input := "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.abc"
	got := Scrub(input)
	want := "Authorization: [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_BearerTokenWithBase64Chars(t *testing.T) {
	input := "Authorization: Bearer abc/def+ghi=="
	got := Scrub(input)
	want := "Authorization: [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_OpenAIKey(t *testing.T) {
	input := "openai=sk-abcdefghijklmnopqrstuvwxyz123456"
	got := Scrub(input)
	want := "openai=[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_PEMKey(t *testing.T) {
	input := "-----BEGIN RSA PRIVATE KEY-----"
	got := Scrub(input)
	want := "[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_SecretRef(t *testing.T) {
	input := "ref: secret://ANTHROPIC_KEY"
	got := Scrub(input)
	want := "ref: [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_NoSecrets(t *testing.T) {
	input := "this is a normal log line"
	got := Scrub(input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestScrub_MultipleSecrets(t *testing.T) {
	input := "keys: sk-ant-abc123 and ghp_token456"
	got := Scrub(input)
	want := "keys: [REDACTED] and [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrubMap_Nested(t *testing.T) {
	input := map[string]any{
		"key":  "sk-ant-abc123",
		"safe": "hello",
		"nested": map[string]any{
			"token": "ghp_secretToken",
		},
		"list": []any{
			"ghs_appToken",
			"safe-value",
		},
		"number": 42.0,
	}

	got := ScrubMap(input)

	if got["key"] != "[REDACTED]" {
		t.Errorf("key: got %q, want [REDACTED]", got["key"])
	}
	if got["safe"] != "hello" {
		t.Errorf("safe: got %q, want hello", got["safe"])
	}
	nested := got["nested"].(map[string]any)
	if nested["token"] != "[REDACTED]" {
		t.Errorf("nested.token: got %q, want [REDACTED]", nested["token"])
	}
	list := got["list"].([]any)
	if list[0] != "[REDACTED]" {
		t.Errorf("list[0]: got %q, want [REDACTED]", list[0])
	}
	if list[1] != "safe-value" {
		t.Errorf("list[1]: got %q, want safe-value", list[1])
	}
	if got["number"] != 42.0 {
		t.Errorf("number: got %v, want 42", got["number"])
	}
}

func TestScrubMap_Nil(t *testing.T) {
	got := ScrubMap(nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestScrubWithStats_NoSecrets(t *testing.T) {
	scrubbed, stats := ScrubWithStats("nothing to redact here")
	if scrubbed != "nothing to redact here" {
		t.Errorf("scrubbed = %q, want unchanged", scrubbed)
	}
	if stats.Count != 0 {
		t.Errorf("stats.Count = %d, want 0", stats.Count)
	}
	if len(stats.Patterns) != 0 {
		t.Errorf("stats.Patterns = %v, want empty", stats.Patterns)
	}
}

func TestScrubWithStats_SingleAnthropicKey(t *testing.T) {
	scrubbed, stats := ScrubWithStats("token sk-ant-abc123")
	if scrubbed != "token [REDACTED]" {
		t.Errorf("scrubbed = %q, want token [REDACTED]", scrubbed)
	}
	if stats.Count != 1 {
		t.Errorf("stats.Count = %d, want 1", stats.Count)
	}
	if len(stats.Patterns) != 1 || stats.Patterns[0] != "anthropic_api_key" {
		t.Errorf("stats.Patterns = %v, want [anthropic_api_key]", stats.Patterns)
	}
}

func TestScrubWithStats_MultipleSecretsCounted(t *testing.T) {
	scrubbed, stats := ScrubWithStats("a=sk-ant-aaa, b=ghp_bbb, c=sk-ant-ccc")
	if scrubbed != "a=[REDACTED], b=[REDACTED], c=[REDACTED]" {
		t.Errorf("scrubbed = %q", scrubbed)
	}
	// 2 anthropic + 1 github = 3 total replacements; 2 distinct pattern names.
	if stats.Count != 3 {
		t.Errorf("stats.Count = %d, want 3", stats.Count)
	}
	if len(stats.Patterns) != 2 {
		t.Errorf("stats.Patterns count = %d (%v), want 2", len(stats.Patterns), stats.Patterns)
	}
}

// TestScrubWithStats_PatternNames asserts that ScrubWithStats reports the
// correct pattern name for every supported secret pattern. The pattern name
// flows into the SecretRedactedInOutput audit event; a typo here breaks
// dashboard queries silently, so it is worth exhaustively enumerated.
//
// The fixtures below are crafted so that exactly one pattern matches each
// input; subsequent patterns run against the already-redacted "[REDACTED]"
// string and (intentionally) do not match it.
func TestScrubWithStats_PatternNames(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		pattern string
	}{
		{
			name:    "anthropic_api_key",
			input:   "sk-ant-abc123_DEF-456",
			pattern: "anthropic_api_key",
		},
		{
			name:    "openai_api_key",
			input:   "sk-abcdefghijklmnop12345",
			pattern: "openai_api_key",
		},
		{
			name:    "github_pat",
			input:   "ghp_abcdefghijklmnop123456",
			pattern: "github_pat",
		},
		{
			name:    "github_app_token",
			input:   "ghs_appTokenValue123",
			pattern: "github_app_token",
		},
		{
			name:    "aws_access_key_id",
			input:   "AKIAIOSFODNN7EXAMPLE",
			pattern: "aws_access_key_id",
		},
		{
			name:    "bearer_token",
			input:   "Bearer eyJhbGciOiJSUzI1NiJ9.abc",
			pattern: "bearer_token",
		},
		{
			name:    "pem_private_key",
			input:   "-----BEGIN RSA PRIVATE KEY-----",
			pattern: "pem_private_key",
		},
		{
			name:    "secret_ref",
			input:   "secret://ANTHROPIC_KEY",
			pattern: "secret_ref",
		},
		{
			name:    "api_key_header_lowercase",
			input:   "api-key: 0123456789abcdef0123456789abcdef",
			pattern: "api_key_header",
		},
		{
			name:    "api_key_header_x_prefix",
			input:   "x-api-key: SOME-VENDOR-VALUE",
			pattern: "api_key_header",
		},
		{
			name:    "api_key_header_apim",
			input:   "Ocp-Apim-Subscription-Key: opaque-token-here",
			pattern: "api_key_header",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stats := ScrubWithStats(tc.input)
			if stats.Count == 0 {
				t.Fatalf("expected at least one redaction for %q, got 0", tc.input)
			}
			if len(stats.Patterns) == 0 || stats.Patterns[0] != tc.pattern {
				t.Errorf("stats.Patterns = %v, want first entry %q", stats.Patterns, tc.pattern)
			}
		})
	}
}

// The legacy Scrub wrapper must still behave identically so existing
// callers are unaffected.
func TestScrubWithStats_ScrubWrapperParity(t *testing.T) {
	cases := []string{
		"plain",
		"sk-ant-xxx",
		"AKIAABCDEFGHIJKLMNOP",
		"Bearer aaa.bbb.ccc",
		"-----BEGIN RSA PRIVATE KEY-----",
		"secret://foo",
	}
	for _, in := range cases {
		got := Scrub(in)
		want, _ := ScrubWithStats(in)
		if got != want {
			t.Errorf("Scrub(%q) = %q; ScrubWithStats(%q) = %q — must match", in, got, in, want)
		}
	}
}

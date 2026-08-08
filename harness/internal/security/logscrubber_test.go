package security

import (
	"strings"
	"testing"
)

func slackTokenFixture() string {
	return "xox" + "b-123456789012-123456789012-abcdefghijklmnopqrstuvwx"
}

func stripeLiveKeyFixture() string {
	return "sk_" + "live_51J1234567890abcdefghijklmnopqrstuvwxyz"
}

func gcpAPIKeyFixture() string {
	return "AI" + "zaA2345678901234567890123456789012345"
}

func TestScrub_AnthropicKey(t *testing.T) {
	input := "key is sk-ant-abc123_DEF-456"
	got := Scrub(input)
	want := "key is [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrub_SecretRefCaseInsensitive pins that the secret_ref pattern
// redacts a "secret://" reference regardless of scheme case, including
// mid-string. This is the belt-and-braces counterpart to
// ValidateRunConfig's case-insensitive hook-command rejection: a
// case-varied reference must not survive Scrub either, since a trace
// consumer (e.g. RecordHookExecution) relies on Scrub as a second line
// of defence rather than trusting the upstream guard alone.
func TestScrub_SecretRefCaseInsensitive(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"uppercase_scheme", "SECRET://API_KEY"},
		{"titlecase_scheme", "Secret://API_KEY"},
		{"mixedcase_scheme", "sEcReT://mixed_case"},
		{"mixedcase_scheme_embedded", "echo sEcReT://mixed_case"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.input)
			if strings.Contains(strings.ToLower(got), "secret://") {
				t.Errorf("Scrub(%q) = %q, want the secret:// reference redacted", tc.input, got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("Scrub(%q) = %q, want a [REDACTED] placeholder", tc.input, got)
			}
		})
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

func TestScrub_AWSSecretAccessKey(t *testing.T) {
	input := "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	got := Scrub(input)
	want := "[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_SlackToken(t *testing.T) {
	input := "SLACK_BOT_TOKEN=" + slackTokenFixture()
	got := Scrub(input)
	want := "SLACK_BOT_TOKEN=[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_StripeLiveKey(t *testing.T) {
	input := "stripe " + stripeLiveKeyFixture()
	got := Scrub(input)
	want := "stripe [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_GCPAPIKey(t *testing.T) {
	input := "gcp=" + gcpAPIKeyFixture()
	got := Scrub(input)
	want := "gcp=[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_AzureStorageKey(t *testing.T) {
	input := "primary_access_key: abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ="
	got := Scrub(input)
	want := "[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_GenericHexSecret(t *testing.T) {
	input := "client_secret=0123456789abcdef0123456789abcdef"
	got := Scrub(input)
	want := "[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrub_AnchoredPatternsAvoidCommonFalsePositives(t *testing.T) {
	cases := []string{
		"sha256=0123456789abcdef0123456789abcdef",
		"trace_id=0123456789abcdef0123456789abcdef",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ=",
	}
	for _, input := range cases {
		if got := Scrub(input); got != input {
			t.Errorf("Scrub(%q) = %q, want unchanged", input, got)
		}
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

// TestScrub_BasicAuth pins the gh-100 SF-2 hardening: Grafana Cloud's
// documented OTLP gateway credential is `Basic <base64>`, not Bearer.
// Without this pattern, a resolved Basic token (e.g. from
// `Authorization: secret://GRAFANA_CLOUD_AUTH`) would survive any slog
// output unscrubbed, defeating the ScrubHandler defence-in-depth
// contract for the new OTLP/HTTP feature.
func TestScrub_BasicAuth(t *testing.T) {
	input := "Authorization: Basic dXNlcjpwYXNz"
	got := Scrub(input)
	want := "Authorization: [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrub_BasicAuth_LongBase64 covers the realistic Grafana Cloud
// shape: `Basic <base64(instanceID:glc_eyJ...)>`. The base64 alphabet
// includes `+/=` which the regex must accept, otherwise long instance
// IDs concatenated with API tokens would only be partially redacted.
func TestScrub_BasicAuth_LongBase64(t *testing.T) {
	input := "Authorization: Basic MTIzNDU2OmdsY19leUp2PT09"
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
			// anthropic_wif_token must take precedence over the generic
			// anthropic_api_key pattern for sk-ant-oat01-* tokens, so
			// audit events distinguish federated leaks from static-key
			// leaks. Order in secretPatterns is load-bearing.
			name:    "anthropic_wif_token",
			input:   "sk-ant-oat01-abcDEF_123-xyz",
			pattern: "anthropic_wif_token",
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
			name:    "aws_secret_access_key",
			input:   "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			pattern: "aws_secret_access_key",
		},
		{
			name:    "azure_storage_key",
			input:   "primary_access_key: abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ=",
			pattern: "azure_storage_key",
		},
		{
			name:    "slack_token",
			input:   slackTokenFixture(),
			pattern: "slack_token",
		},
		{
			name:    "stripe_live_key",
			input:   stripeLiveKeyFixture(),
			pattern: "stripe_live_key",
		},
		{
			name:    "gcp_api_key",
			input:   gcpAPIKeyFixture(),
			pattern: "gcp_api_key",
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
		{
			name:    "oidc_jwt",
			input:   "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.abc123",
			pattern: "oidc_jwt",
		},
		{
			name:    "gcp_access_token",
			input:   "ya29.AbCdEfGhIjKl",
			pattern: "gcp_access_token",
		},
		{
			name:    "generic_hex_secret",
			input:   "client_secret=0123456789abcdef0123456789abcdef",
			pattern: "generic_hex_secret",
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

// TestScrub_OIDCJWT exercises the federation-token redaction pattern.
// Federation error paths (truncateForError, GHA OIDC error body, Azure
// IMDS error body) embed the subject token verbatim if the upstream
// endpoint echoes it; without this scrubber entry, a hostile STS would
// leak a usable JWT into slog/OTel output.
func TestScrub_OIDCJWT(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.abc123"
	input := "got " + jwt + " from endpoint"
	got := Scrub(input)
	want := "got [REDACTED] from endpoint"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrub_OIDCJWTNoBoundary asserts the JWT redaction does not
// over-extend past the token. Adjacent text on either side must
// survive intact so error messages remain debuggable.
func TestScrub_OIDCJWTNoBoundary(t *testing.T) {
	input := "prefix eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.signature suffix"
	got := Scrub(input)
	want := "prefix [REDACTED] suffix"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrub_GCPAccessToken exercises the ya29.* prefix common to all
// Google OAuth2 access tokens (Application Default Credentials, IAM
// generateAccessToken impersonation results, federated tokens).
func TestScrub_GCPAccessToken(t *testing.T) {
	input := "token ya29.AbCdEfGhIjKl"
	got := Scrub(input)
	want := "token [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrub_GCPAccessTokenWithSurroundingText asserts the redaction
// stops at any character outside the base64url alphabet so trailing
// punctuation does not get pulled into the [REDACTED] span.
func TestScrub_GCPAccessTokenWithSurroundingText(t *testing.T) {
	input := `prefix ya29.AbCdEfGhIjKl and "ya29.MoreToken_Here-x" plus more`
	got := Scrub(input)
	want := `prefix [REDACTED] and "[REDACTED]" plus more`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
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

// TestScrub_SkPrefixRequiresWordBoundary pins the \b anchor on the
// three sk- secret patterns. The unanchored form matched the tail of
// any hyphenated identifier ending in "...sk-" — "ta|sk" carries no
// word boundary — which meant the eval runner's own workspace paths
// registered as OpenAI API keys. Because sensitivepatterns.go lifts
// these regexes into the Rule-of-Two detector at TierLatch, that false
// positive revoked run_command mid-run and made the eval gate flaky
// across three different models.
func TestScrub_SkPrefixRequiresWordBoundary(t *testing.T) {
	notSecrets := []string{
		// The exact string that broke ci.yml::eval-gate.
		"/tmp/eval-task-narrow-edit-leaves-surroundings-alone-1530359655/target.txt",
		// Ordinary English words ending in "sk" are the general case.
		"stat /var/task-management-system-v2-config/main.go",
		"disk-usage-monitoring-daemon-v3",
		"at risk-assessment-framework-2026 line 4",
	}
	for _, in := range notSecrets {
		if got := Scrub(in); got != in {
			t.Errorf("Scrub(%q) = %q, want unchanged — an identifier containing \"sk-\" mid-word is not a key", in, got)
		}
	}

	// The anchor must not cost a real key. Every realistic occurrence
	// is preceded by start-of-string or a non-word delimiter.
	realKeys := []string{
		"sk-abcdefghijklmnop12345",
		"OPENAI_API_KEY=sk-abcdefghijklmnop12345",
		"Authorization: sk-ant-abc123_DEF-456",
		`{"key":"sk-ant-oat01-abcDEF_123-xyz"}`,
		"token is sk-abcdefghijklmnop12345, rotate it",
	}
	for _, in := range realKeys {
		got := Scrub(in)
		if strings.Contains(got, "sk-") {
			t.Errorf("Scrub(%q) = %q, want the key redacted", in, got)
		}
	}
}

// The Rule-of-Two detector is the consumer that made this a run-breaking
// bug rather than a cosmetic over-redaction, so pin it there too: a
// workspace path must not produce a latching finding.
func TestDetectSensitive_WorkspacePathIsNotAKey(t *testing.T) {
	path := "Tool error: stat file: stat /tmp/eval-task-narrow-edit-leaves-surroundings-alone-1530359655/target.txt: no such file or directory"
	if fs := DetectSensitive(path); len(fs) != 0 {
		t.Errorf("DetectSensitive(%q) = %+v, want no findings — a tool-result path must not latch Rule-of-Two", path, fs)
	}

	// Control: a genuine key in a tool result still latches.
	leaked := "cat .env: OPENAI_API_KEY=sk-abcdefghijklmnop12345"
	fs := DetectSensitive(leaked)
	if len(fs) == 0 {
		t.Fatalf("DetectSensitive(%q) returned no findings, want the key detected", leaked)
	}
	var latched bool
	for _, f := range fs {
		if f.Tier == TierLatch {
			latched = true
		}
	}
	if !latched {
		t.Errorf("DetectSensitive(%q) = %+v, want a TierLatch finding", leaked, fs)
	}
}

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

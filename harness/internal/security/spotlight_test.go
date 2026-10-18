package security

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestScrubFenced_ScrubsFencePayloads(t *testing.T) {
	key := "sk-" + "ant-api03-QUJDZGVmR0hJSktMbW5vcFFSU1RVdld4eXow"
	cases := map[string]struct{ in, want string }{
		"span fence":       {"prefix " + Spotlight(key) + " suffix", "prefix " + Spotlight("[REDACTED]") + " suffix"},
		"whole-block":      {Spotlight("line one\nkey " + key + "\nline three"), Spotlight("line one\nkey [REDACTED]\nline three")},
		"nested fence":     {Spotlight("outer " + Spotlight(key)), Spotlight("outer " + Spotlight("[REDACTED]"))},
		"benign fence":     {"see " + Spotlight("plain words, nothing secret"), "see " + Spotlight("plain words, nothing secret")},
		"forged plaintext": {SpotlightOpenTag + key + SpotlightCloseTag, SpotlightOpenTag + "[REDACTED]" + SpotlightCloseTag},
		"unclosed fence":   {SpotlightOpenTag + key, SpotlightOpenTag + "[REDACTED]"},
		"plain text":       {"key " + key, "key [REDACTED]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ScrubFenced(tc.in)
			if got != tc.want {
				t.Errorf("ScrubFenced = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, base64.StdEncoding.EncodeToString([]byte(key))) {
				t.Errorf("base64 of the key survived: %q", got)
			}
		})
	}
}

func TestDecodeFences(t *testing.T) {
	cases := map[string]struct {
		in, want string
		ok       bool
	}{
		"plain":            {"no fences", "no fences", true},
		"two fences":       {"a " + Spotlight("x") + " b " + Spotlight("y"), "a x b y", true},
		"unclosed":         {"a " + SpotlightOpenTag + "eA==", "", false},
		"stray close":      {"a " + SpotlightCloseTag + " b", "", false},
		"not base64":       {SpotlightOpenTag + "not base64!" + SpotlightCloseTag, "", false},
		"close before one": {SpotlightCloseTag + Spotlight("x"), "", false},
	}
	for name, tc := range cases {
		if got, ok := DecodeFences(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("%s: DecodeFences = %q, %v; want %q, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

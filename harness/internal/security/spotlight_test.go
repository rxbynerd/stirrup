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
		"stray open tag before a fence": {
			"README mentions " + SpotlightOpenTag + " tags.\nGITHUB_TOKEN=" + Spotlight(key) + "\n",
			"README mentions " + SpotlightOpenTag + " tags.\nGITHUB_TOKEN=" + Spotlight("[REDACTED]") + "\n",
		},
		"stray base64 open tag before a fence": {
			SpotlightOpenTag + "QUJD " + Spotlight(key),
			SpotlightOpenTag + "QUJD " + Spotlight("[REDACTED]"),
		},
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

// spotlightRanges fences the ranges the scrubber replaces in value, as the
// loop does for a tool result, and returns the fenced text and the
// original text of each range.
func spotlightRanges(value string) (string, []string) {
	_, ranges, _ := ScrubWithSpans(value)
	var b strings.Builder
	var secrets []string
	prev := 0
	for _, r := range ranges {
		b.WriteString(value[prev:r.Start])
		b.WriteString(Spotlight(value[r.Start:r.End]))
		secrets = append(secrets, value[r.Start:r.End])
		prev = r.End
	}
	b.WriteString(value[prev:])
	return b.String(), secrets
}

// FuzzScrubFenced checks that persisting fenced text never keeps a fenced
// secret, raw or base64, whatever plain text, stray tags included, comes
// before it.
func FuzzScrubFenced(f *testing.F) {
	key := "sk-" + "ant-api03-QUJDZGVmR0hJSktMbW5vcFFSU1RVdld4eXow"
	f.Add("README mentions "+SpotlightOpenTag+" tags.\n", "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n")
	f.Add(SpotlightOpenTag+"not base64!! ", "key "+key)
	f.Add(SpotlightOpenTag+"QUJD", "Bearer abc.def "+key)
	f.Add(SpotlightCloseTag+SpotlightOpenTag, "x "+key+" y secret://ref")
	f.Fuzz(func(t *testing.T, prefix, value string) {
		fenced, secrets := spotlightRanges(value)
		out := ScrubFenced(prefix + fenced)
		for _, s := range secrets {
			if len(s) < 8 {
				continue
			}
			if strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(s))) {
				t.Fatalf("base64 of fenced secret %q persisted: %q", s, out)
			}
			if strings.Count(prefix+value, s) == 1 && strings.Contains(out, s) {
				t.Fatalf("fenced secret %q persisted raw: %q", s, out)
			}
		}
	})
}

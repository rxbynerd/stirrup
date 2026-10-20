package security

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func hasFinding(fs []SensitiveFinding, name string) bool {
	for _, f := range fs {
		if f.Name == name {
			return true
		}
	}
	return false
}

func findByName(fs []SensitiveFinding, name string) (SensitiveFinding, bool) {
	for _, f := range fs {
		if f.Name == name {
			return f, true
		}
	}
	return SensitiveFinding{}, false
}

// The canonical test PAN 4111111111111111 SHOULD latch: the detector
// cannot distinguish documented test numbers from real cards, and
// latching on Luhn-valid fixtures is the documented operator footgun.
func TestDetectSensitive_CreditCardLuhnValid(t *testing.T) {
	must := require.New(t)
	fs := DetectSensitive("payment card 4111111111111111 on file")
	f, ok := findByName(fs, "pii/credit_card")
	must.True(ok, "expected pii/credit_card finding, got %v", fs)
	must.Equal(TierLatch, f.Tier)
}

func TestDetectSensitive_CreditCardLuhnInvalid(t *testing.T) {
	must := require.New(t)
	fs := DetectSensitive("invoice ref 4111111111111112 cleared")
	must.False(hasFinding(fs, "pii/credit_card"), "Luhn-invalid number produced a finding: %v", fs)
}

// Nil UUIDs and zero-filled placeholders satisfy the Luhn checksum
// (sum 0), but no issued card starts with 0.
func TestDetectSensitive_CreditCardZeroFilled(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{name: "nil uuid", input: "tenant 00000000-0000-0000-0000-000000000000 selected"},
		{name: "zero-filled masked card", input: "masked card 0000 0000 0000 0000 on record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			fs := DetectSensitive(tc.input)
			must.False(hasFinding(fs, "pii/credit_card"), "credit-card finding: %v", fs)
		})
	}
}

func TestDetectSensitive_CreditCardSeparators(t *testing.T) {
	for _, tc := range []struct{ name, card string }{
		{name: "space separators", card: "4111 1111 1111 1111"},
		{name: "dash separators", card: "4111-1111-1111-1111"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			content := "card " + tc.card + " expires soon"
			fs := DetectSensitive(content)
			f, ok := findByName(fs, "pii/credit_card")
			must.True(ok, "expected pii/credit_card finding, got %v", fs)
			must.Equal(tc.card, content[f.Start:f.End])
		})
	}
}

func TestDetectSensitive_IBAN(t *testing.T) {
	t.Run("valid iban latches", func(t *testing.T) {
		must := require.New(t)
		fs := DetectSensitive("transfer to GB82WEST12345698765432 today")
		f, ok := findByName(fs, "pii/iban")
		must.True(ok, "expected pii/iban finding, got %v", fs)
		must.Equal(TierLatch, f.Tier)
	})

	t.Run("mod-97-broken iban is ignored", func(t *testing.T) {
		must := require.New(t)
		fs := DetectSensitive("transfer to GB82WEST12345698765431 today")
		must.False(hasFinding(fs, "pii/iban"), "mod-97-broken IBAN produced a finding: %v", fs)
	})
}

func TestDetectSensitive_SSNAnchored(t *testing.T) {
	must := require.New(t)
	fs := DetectSensitive("Customer SSN: 123-45-6789 verified")
	f, ok := findByName(fs, "pii/ssn_anchored")
	must.True(ok, "expected pii/ssn_anchored finding, got %v", fs)
	must.Equal(TierLatch, f.Tier)
	must.False(hasFinding(fs, "pii/ssn_bare"), "anchored SSN also produced pii/ssn_bare: %v", fs)
}

func TestDetectSensitive_SSNBare(t *testing.T) {
	must := require.New(t)
	fs := DetectSensitive("value 123-45-6789 recorded")
	f, ok := findByName(fs, "pii/ssn_bare")
	must.True(ok, "expected pii/ssn_bare finding, got %v", fs)
	must.Equal(TierWarn, f.Tier)
	must.False(hasFinding(fs, "pii/ssn_anchored"), "bare SSN also produced pii/ssn_anchored: %v", fs)
}

func TestDetectSensitive_SSNAnchorOutsideWindow(t *testing.T) {
	must := require.New(t)
	content := "ssn " + strings.Repeat("x", 70) + " 123-45-6789"
	fs := DetectSensitive(content)
	must.False(hasFinding(fs, "pii/ssn_anchored"), "anchor beyond the ±64 window still latched: %v", fs)
	must.True(hasFinding(fs, "pii/ssn_bare"), "expected pii/ssn_bare finding, got %v", fs)
}

func TestDetectSensitive_EmailBulk(t *testing.T) {
	t.Run("five distinct addresses fire", func(t *testing.T) {
		must := require.New(t)
		five := "a1@example.com b2@example.com c3@example.com d4@example.com e5@example.com"
		fs := DetectSensitive(five)
		f, ok := findByName(fs, "pii/email_bulk")
		must.True(ok, "expected pii/email_bulk finding, got %v", fs)
		must.Equal(TierWarn, f.Tier)
		must.Equal("a1@example.com", five[f.Start:f.End])
	})

	t.Run("four distinct addresses do not fire", func(t *testing.T) {
		must := require.New(t)
		four := "a1@example.com b2@example.com c3@example.com d4@example.com"
		fs := DetectSensitive(four)
		must.False(hasFinding(fs, "pii/email_bulk"), "4 distinct addresses fired: %v", fs)
	})

	t.Run("repeats of one address do not fire", func(t *testing.T) {
		must := require.New(t)
		repeated := strings.Repeat("a1@example.com ", 9)
		fs := DetectSensitive(repeated)
		must.False(hasFinding(fs, "pii/email_bulk"), "repeats of one address fired: %v", fs)
	})

	t.Run("case variants of one address are not distinct", func(t *testing.T) {
		must := require.New(t)
		caseFolded := "A1@Example.com a1@example.COM b2@example.com c3@example.com d4@example.com"
		fs := DetectSensitive(caseFolded)
		must.False(hasFinding(fs, "pii/email_bulk"), "case variants of one address counted as distinct: %v", fs)
	})
}

func TestDetectSensitive_SecretPattern(t *testing.T) {
	must := require.New(t)
	fs := DetectSensitive("key is sk-ant-" + "abc123_DEF-456")
	f, ok := findByName(fs, "secret/anthropic_api_key")
	must.True(ok, "expected secret/anthropic_api_key finding, got %v", fs)
	must.Equal(TierLatch, f.Tier)
}

func TestDetectSensitive_ExampleKeyAllowlist(t *testing.T) {
	must := require.New(t)
	for _, tc := range []struct{ name, key string }{
		{name: "docs example 1", key: "AKIAIOSFODNN7EXAMPLE"},
		{name: "docs example 2", key: "AKIAI44QH8DHBEXAMPLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			fs := DetectSensitive("old key was " + tc.key + ", revoked")
			must.Empty(fs, "doc example %s produced findings", tc.key)
		})
	}

	realKey := "AKIA" + "ABCDEFGHIJKLMNOP"
	fs := DetectSensitive("found " + realKey + " in .env")
	must.True(hasFinding(fs, "secret/aws_access_key_id"), "non-example AKIA key not detected: %v", fs)
}

// Greedy variable-length patterns absorb an appended EXAMPLE into the
// match, so the exclusion is exact-literal: suffixing a real key with
// EXAMPLE must still latch.
func TestDetectSensitive_ExampleSuffixBypassPrevented(t *testing.T) {
	t.Run("github pat with EXAMPLE suffix", func(t *testing.T) {
		must := require.New(t)
		realGHP := "ghp_" + "myrealtoken1234"
		fs := DetectSensitive("key: " + realGHP + "EXAMPLE")
		must.True(hasFinding(fs, "secret/github_pat"), "not detected: %v", fs)
	})

	t.Run("anthropic key with EXAMPLE suffix", func(t *testing.T) {
		must := require.New(t)
		realAnthropic := "sk-ant-" + "abc123DEF456xyz789AB"
		fs := DetectSensitive(realAnthropic + "EXAMPLE")
		must.True(hasFinding(fs, "secret/anthropic_api_key"), "not detected: %v", fs)
	})

	// Fixed-length AKIA cannot absorb the suffix: the appended EXAMPLE
	// sits after the 20-char match and must not suppress it.
	t.Run("aws access key with EXAMPLE suffix", func(t *testing.T) {
		must := require.New(t)
		realAKIA := "AKIA" + "ABCDEFGHIJKLMNOP"
		fs := DetectSensitive("leak " + realAKIA + "EXAMPLE")
		must.True(hasFinding(fs, "secret/aws_access_key_id"), "not detected: %v", fs)
	})
}

func TestDetectSensitive_SpanOffsets(t *testing.T) {
	must := require.New(t)
	key := "AKIA" + "ABCDEFGHIJKLMNOP"
	content := "before " + key + " after"
	fs := DetectSensitive(content)
	f, ok := findByName(fs, "secret/aws_access_key_id")
	must.True(ok, "expected secret/aws_access_key_id finding, got %v", fs)
	must.Equal(key, content[f.Start:f.End])
}

// aws_secret_access_key has a (?i) prefix, so its lowerRe is non-nil and
// this exercises the lowerRe scan path, whose offsets drive redaction.
// Uppercase characters before the key pin the byte-preserving property
// of asciiLower: replacing it with anything that can change byte length
// would shift the span and fail here.
func TestDetectSensitive_SpanOffsets_LowerRePath(t *testing.T) {
	must := require.New(t)
	key := strings.Repeat("Ab1+", 10)
	content := "BEFORE AWS_SECRET_ACCESS_KEY = " + key + " AFTER"
	fs := DetectSensitive(content)
	f, ok := findByName(fs, "secret/aws_secret_access_key")
	must.True(ok, "expected secret/aws_secret_access_key finding, got %v", fs)
	got := content[f.Start:f.End]
	must.True(strings.HasPrefix(got, "AWS_SECRET_ACCESS_KEY"), "lowerRe span sliced %q", got)
	must.True(strings.HasSuffix(got, key), "lowerRe span sliced %q", got)
}

// A secret:// reference is a pointer, not key material, so it reports at
// warn tier.
func TestDetectSensitive_SecretRefIsWarn(t *testing.T) {
	must := require.New(t)
	fs := DetectSensitive(`"apiKeyRef": "secret://ANTHROPIC_API_KEY"`)
	f, ok := findByName(fs, "secret/secret_ref")
	must.True(ok, "expected secret/secret_ref finding, got %v", fs)
	must.Equal(TierWarn, f.Tier)
}

func TestDetectSensitive_BearerProse(t *testing.T) {
	t.Run("prose mention does not latch", func(t *testing.T) {
		must := require.New(t)
		fs := DetectSensitive("Use Bearer tokens for HTTP authentication")
		must.False(hasFinding(fs, "secret/bearer_token"), "prose mention of bearer tokens latched: %v", fs)
	})

	t.Run("credential-shaped token is detected", func(t *testing.T) {
		must := require.New(t)
		fs := DetectSensitive("Authorization: Bearer " + "a1b2c3d4e5f6g7h8i9j0k1")
		must.True(hasFinding(fs, "secret/bearer_token"), "credential-shaped bearer token not detected: %v", fs)
	})
}

func TestDetectSensitive_BasicAuthProse(t *testing.T) {
	t.Run("prose mention does not latch", func(t *testing.T) {
		must := require.New(t)
		fs := DetectSensitive("Basic auth is the simplest scheme")
		must.False(hasFinding(fs, "secret/basic_auth"), "prose mention of basic auth latched: %v", fs)
	})

	t.Run("real basic credential is detected", func(t *testing.T) {
		must := require.New(t)
		payload := base64.StdEncoding.EncodeToString([]byte("123456:" + "glc_token12345"))
		fs := DetectSensitive("Authorization: Basic " + payload)
		must.True(hasFinding(fs, "secret/basic_auth"), "real basic credential not detected: %v", fs)
	})
}

// TestBasicAuth_DetectorAndScrubberAgree pins that every long Basic payload
// the detector latches on is also redacted by the scrubber.
func TestBasicAuth_DetectorAndScrubberAgree(t *testing.T) {
	for name, raw := range map[string]string{
		"latin-1 password": "user:p\xe4ssword12345",
		"tab in password":  "user:pass\tword12345",
		"trailing newline": "user:password12345\n",
		"binary password":  "user:\x00\x01\xff\xfepassword",
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			input := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
			fs := DetectSensitive(input)
			must.True(hasFinding(fs, "secret/basic_auth"), "detector missed %q", input)
			must.Equal("Authorization: [REDACTED]", Scrub(input))
		})
	}
}

func TestDetectSensitive_GrafanaDocExampleAllowlisted(t *testing.T) {
	must := require.New(t)
	fs := DetectSensitive(`auth: "Basic MTIzNDU2OmdsY18uLi4="`)
	must.False(hasFinding(fs, "secret/basic_auth"), "docs/observability-cloud.md example latched: %v", fs)
}

// TestDetectSensitive_PEMBodyWithDocLiteralStillLatches pins that the
// documentation-literal exemption cannot be reached through a key body: a
// literal placed on one body line must not suppress the key's finding.
func TestDetectSensitive_PEMBodyWithDocLiteralStillLatches(t *testing.T) {
	must := require.New(t)
	body := pemBodyLines(6)
	body[3] = "MTIzNDU2OmdsY18uLi4=" + body[3][20:]
	fs := DetectSensitive("cat id_rsa\n" + pemKey("RSA PRIVATE KEY", body, "\n") + "\n")
	latched := slices.ContainsFunc(fs, func(f SensitiveFinding) bool {
		return f.Name == "secret/pem_private_key" && f.Tier == TierLatch
	})
	must.True(latched, "no latch-tier pem_private_key finding for a key body holding a doc literal: %v", fs)

	bare := DetectSensitive("Basic MTIzNDU2OmdsY18uLi4=")
	must.False(hasFinding(bare, "secret/basic_auth"), "bare documentation literal latched: %v", bare)
}

func TestDetectSensitive_APIKeyHeaderSecretRef(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{name: "dd-api-key reference", input: "dd-api-key: secret://DD_API_KEY"},
		{name: "x-api-key reference", input: "x-api-key: secret://X_API_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			for _, f := range DetectSensitive(tc.input) {
				must.NotEqual(TierLatch, f.Tier, "secret:// header value produced latch finding %s", f.Name)
			}
		})
	}

	t.Run("real api-key value is detected", func(t *testing.T) {
		must := require.New(t)
		hex32 := strings.Repeat("0123456789abcdef", 2)
		fs := DetectSensitive("api-key: " + hex32)
		must.True(hasFinding(fs, "secret/api_key_header"), "real api-key header value not detected: %v", fs)
	})
}

// Only the bare secret:// reference shape may suppress a match followed
// by "://"; any other value followed by "://" must still latch.
func TestDetectSensitive_APIKeyHeaderURLSuffixBypassPrevented(t *testing.T) {
	t.Run("key followed by :// is still detected", func(t *testing.T) {
		must := require.New(t)
		realKey := strings.Repeat("0123456789abcdef", 2)
		fs := DetectSensitive("api-key: " + realKey + "://api.attacker.example/steal")
		must.True(hasFinding(fs, "secret/api_key_header"), "api-key value followed by :// was suppressed: %v", fs)
	})

	// A value that merely ends in the word "secret" is still potential
	// key material; only the exact scheme word is a reference.
	t.Run("value ending in secret is still detected", func(t *testing.T) {
		must := require.New(t)
		fs := DetectSensitive("api-key: " + "a1b2c3d4e5f6secret" + "://api.attacker.example/steal")
		must.True(hasFinding(fs, "secret/api_key_header"), "api-key value ending in 'secret' followed by :// was suppressed: %v", fs)
	})
}

func TestDetectSensitive_Empty(t *testing.T) {
	must := require.New(t)
	must.Nil(DetectSensitive(""))
}

// Mixed-case inputs exercise the ASCII-lowered fast path that replaces
// the canonical (?i) regexes; missing them here means the lowered
// variants drifted from the scrubber sources.
func TestDetectSensitive_CaseInsensitivePatternsViaLoweredPath(t *testing.T) {
	t.Run("uppercase bearer credential", func(t *testing.T) {
		must := require.New(t)
		fs := DetectSensitive("Authorization: BEARER " + "A1B2C3D4E5F6G7H8I9J0")
		must.True(hasFinding(fs, "secret/bearer_token"), "uppercase bearer credential not detected: %v", fs)
	})

	t.Run("uppercase aws secret key assignment", func(t *testing.T) {
		must := require.New(t)
		val := strings.Repeat("Ab1+", 10)
		fs := DetectSensitive("AWS_SECRET_ACCESS_KEY = " + val)
		must.True(hasFinding(fs, "secret/aws_secret_access_key"), "uppercase aws_secret_access_key assignment not detected: %v", fs)
	})

	t.Run("uppercase generic hex secret", func(t *testing.T) {
		must := require.New(t)
		hex32 := strings.Repeat("0123456789ABCDEF", 2)
		fs := DetectSensitive("PASSWORD: " + hex32)
		must.True(hasFinding(fs, "secret/generic_hex_secret"), "uppercase generic hex secret not detected: %v", fs)
	})
}

// Every prefilter literal must keep appearing in its scrubber regex
// source: the prefilters skip the regex when no literal is present, so
// a scrubber rename that drops a literal would silently disable
// detection rather than fail loudly.
func TestPrefilterLiterals_StayInScrubberSources(t *testing.T) {
	sources := map[string]string{}
	for _, p := range SecretPatterns() {
		sources[p.Name] = strings.ToLower(p.Re.String())
	}
	for name, lits := range map[string][]string{
		"aws_secret_access_key": awsSecretPrefilterLits,
		"azure_storage_key":     azureKeyPrefilterLits,
		"api_key_header":        apiKeyHeaderPrefilterLits,
		"generic_hex_secret":    genericHexPrefilterLits,
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			src, ok := sources[name]
			must.True(ok, "scrubber pattern %q no longer exists", name)
			for _, lit := range lits {
				must.Contains(src, lit)
			}
		})
	}
}

// Every derived lowerRe must agree with its canonical (?i) regex —
// same matches, same byte offsets — over a corpus that actually
// exercises the six case-insensitive patterns in mixed case. A
// divergence means lowerVariant mis-lowered a construct.
func TestLowerVariant_EquivalentToCanonical(t *testing.T) {
	must := require.New(t)
	samples := []string{
		"AWS_SECRET_ACCESS_KEY = " + strings.Repeat("Ab1+", 10),
		"aws_secret_access_key: '" + strings.Repeat("zZ9/", 10) + "'",
		"Authorization: BEARER A1b2C3d4E5f6G7h8I9j0",
		"bearer tokens are discussed here",
		"Basic " + "YWxhZGRpbjpvcGVuU2VzYW1l",
		"BASIC AUTH IS SIMPLE",
		"X-API-KEY: 0123456789ABCDEF0123456789abcdef",
		"Ocp-Apim-Subscription-Key: AbCd1234efGh5678",
		"PASSWORD = \"0123456789abcdefABCDEF0123456789\"",
		"accountKey=" + strings.Repeat("Qq+/", 11),
		"no credentials in this line at all",
	}
	var corpus []string
	for _, s := range samples {
		corpus = append(corpus, s, "PREFIX "+s+" suffix", strings.ToUpper(s), strings.ToLower(s))
	}
	withLowerRe := 0
	for _, p := range sensitivePatterns {
		if p.lowerRe == nil {
			continue
		}
		withLowerRe++
		t.Run(p.Name, func(t *testing.T) {
			must := require.New(t)
			for _, s := range corpus {
				canonical := p.Re.FindAllStringIndex(s, -1)
				lowered := p.lowerRe.FindAllStringIndex(asciiLower(s), -1)
				must.Equal(canonical, lowered, "diverges on %q", s)
			}
		})
	}
	must.Positive(withLowerRe, "no pattern has a lowerRe; the fast path is dead and this test is vacuous")
}

// Ranges within one case lower cleanly and must stay eligible; the (?i)
// scrubber patterns rely on them.
func TestHasUnsafeClassRange_FlagsCaseCrossingRanges(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{name: "digit to uppercase", src: `[0-F]`, want: true},
		{name: "uppercase to lowercase spanning punctuation", src: `[A-z]`, want: true},
		{name: "punctuation to uppercase", src: `[?-Z]`, want: true},
		{name: "uppercase to lowercase across the bracket gap", src: `[Z-a]`, want: true},
		{name: "uppercase range", src: `[A-Z]`, want: false},
		{name: "mixed-case class", src: `[A-Za-z0-9+/]`, want: false},
		{name: "lowercase range", src: `[a-z0-9]`, want: false},
		{name: "full scrubber pattern", src: `\baws\b\s*[:=]\s*[A-Za-z0-9/+=]{40}`, want: false},
		{name: "hyphen outside a class", src: `abc-DEF`, want: false},
		{name: "escaped hyphen in a class", src: `[a\-Z]`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, hasUnsafeClassRange(tc.src))
		})
	}
}

// A regex that can match the empty string must not pin detectBulk in
// place; emailRe cannot produce one today, but a future MinDistinct
// pattern with a *-quantifier would otherwise infinite-loop on every
// tool result.
func TestDetectBulk_ZeroLengthMatchTerminates(t *testing.T) {
	must := require.New(t)
	p := &SensitivePattern{Name: "test/zero", Re: regexp.MustCompile(`a*`), Tier: TierWarn, MinDistinct: 3}
	f, ok := detectBulk("bbbb", p)
	must.False(ok, "only the empty string matches, expected no finding, got %v", f)
	_, ok = detectBulk("xaxbxaax", p)
	must.True(ok, `expected a finding: "", "a", "aa" are three distinct matches`)
}

// The DetectSensitive memo caches scan results keyed on the p.Re
// pointer alone. Correctness requires that consecutive patterns
// sharing a *regexp.Regexp also agree on lowerRe (both nil or both the
// same), or one pattern would silently receive matches computed
// against the wrong haystack.
func TestSensitivePatterns_SharedRegexpAgreesOnLowerRe(t *testing.T) {
	must := require.New(t)
	for i := 1; i < len(sensitivePatterns); i++ {
		a, b := sensitivePatterns[i-1], sensitivePatterns[i]
		if a.Re != b.Re {
			continue
		}
		must.Same(a.lowerRe, b.lowerRe, "patterns %s and %s share Re but differ on lowerRe; memo cache would serve wrong matches", a.Name, b.Name)
	}
}

// benchmarkCorpus builds a deterministic mixed corpus: prose that
// exercises the bearer/basic prose-rejection validators, JSON-ish log
// lines with emails (repeats of one address, below the bulk
// threshold), hex IDs, base64 blobs, and digit runs that never pass
// Luhn.
func benchmarkCorpus(size int) string {
	lines := []string{
		"2026-06-07T12:00:00Z INFO request completed status=200 dur=15ms trace=4bf92f3577b34da6a3ce929d0e0e4736",
		"The quick brown fox discusses Basic auth and Bearer tokens over coffee.",
		`{"user":"alice","email":"alice@example.com","note":"order 1234 shipped"}`,
		"commit 9fceb02d0ae598e95dc970b74767f19372d61af8 refactor: extract helper",
		"data: SGVsbG8gd29ybGQgdGhpcyBpcyBhIHRlc3Q=",
		"GET /api/v1/items?page=3&limit=50 HTTP/1.1",
		"invoice ref 4111111111111112 cleared; total 123456789",
		"Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor.",
	}
	var b strings.Builder
	b.Grow(size + 128)
	for i := 0; b.Len() < size; i++ {
		b.WriteString(lines[i%len(lines)])
		b.WriteByte('\n')
	}
	return b.String()[:size]
}

func BenchmarkDetectSensitive_1MB(b *testing.B) {
	corpus := benchmarkCorpus(1 << 20)
	b.SetBytes(1 << 20)
	b.ReportAllocs()
	for b.Loop() {
		DetectSensitive(corpus)
	}
}

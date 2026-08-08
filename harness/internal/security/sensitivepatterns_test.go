package security

import (
	"encoding/base64"
	"reflect"
	"regexp"
	"strings"
	"testing"
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
	fs := DetectSensitive("payment card 4111111111111111 on file")
	f, ok := findByName(fs, "pii/credit_card")
	if !ok {
		t.Fatalf("expected pii/credit_card finding, got %v", fs)
	}
	if f.Tier != TierLatch {
		t.Errorf("tier = %q, want %q", f.Tier, TierLatch)
	}
}

func TestDetectSensitive_CreditCardLuhnInvalid(t *testing.T) {
	fs := DetectSensitive("invoice ref 4111111111111112 cleared")
	if hasFinding(fs, "pii/credit_card") {
		t.Errorf("Luhn-invalid number produced a finding: %v", fs)
	}
}

// Nil UUIDs and zero-filled placeholders satisfy the Luhn checksum
// (sum 0) but no issued card starts with 0; found live in
// examples/runconfig/azure-openai-wif-usgov.json during the
// repo-corpus false-positive pass.
func TestDetectSensitive_CreditCardZeroFilled(t *testing.T) {
	for _, input := range []string{
		"tenant 00000000-0000-0000-0000-000000000000 selected",
		"masked card 0000 0000 0000 0000 on record",
	} {
		if fs := DetectSensitive(input); hasFinding(fs, "pii/credit_card") {
			t.Errorf("input %q produced a credit-card finding: %v", input, fs)
		}
	}
}

func TestDetectSensitive_CreditCardSeparators(t *testing.T) {
	for _, input := range []string{
		"4111 1111 1111 1111",
		"4111-1111-1111-1111",
	} {
		fs := DetectSensitive("card " + input + " expires soon")
		f, ok := findByName(fs, "pii/credit_card")
		if !ok {
			t.Errorf("input %q: expected pii/credit_card finding, got %v", input, fs)
			continue
		}
		if got := ("card " + input + " expires soon")[f.Start:f.End]; got != input {
			t.Errorf("input %q: span sliced %q", input, got)
		}
	}
}

func TestDetectSensitive_IBAN(t *testing.T) {
	fs := DetectSensitive("transfer to GB82WEST12345698765432 today")
	f, ok := findByName(fs, "pii/iban")
	if !ok {
		t.Fatalf("expected pii/iban finding, got %v", fs)
	}
	if f.Tier != TierLatch {
		t.Errorf("tier = %q, want %q", f.Tier, TierLatch)
	}

	fs = DetectSensitive("transfer to GB82WEST12345698765431 today")
	if hasFinding(fs, "pii/iban") {
		t.Errorf("mod-97-broken IBAN produced a finding: %v", fs)
	}
}

func TestDetectSensitive_SSNAnchored(t *testing.T) {
	fs := DetectSensitive("Customer SSN: 123-45-6789 verified")
	f, ok := findByName(fs, "pii/ssn_anchored")
	if !ok {
		t.Fatalf("expected pii/ssn_anchored finding, got %v", fs)
	}
	if f.Tier != TierLatch {
		t.Errorf("tier = %q, want %q", f.Tier, TierLatch)
	}
	if hasFinding(fs, "pii/ssn_bare") {
		t.Errorf("anchored SSN also produced pii/ssn_bare: %v", fs)
	}
}

func TestDetectSensitive_SSNBare(t *testing.T) {
	fs := DetectSensitive("value 123-45-6789 recorded")
	f, ok := findByName(fs, "pii/ssn_bare")
	if !ok {
		t.Fatalf("expected pii/ssn_bare finding, got %v", fs)
	}
	if f.Tier != TierWarn {
		t.Errorf("tier = %q, want %q", f.Tier, TierWarn)
	}
	if hasFinding(fs, "pii/ssn_anchored") {
		t.Errorf("bare SSN also produced pii/ssn_anchored: %v", fs)
	}
}

func TestDetectSensitive_SSNAnchorOutsideWindow(t *testing.T) {
	content := "ssn " + strings.Repeat("x", 70) + " 123-45-6789"
	fs := DetectSensitive(content)
	if hasFinding(fs, "pii/ssn_anchored") {
		t.Errorf("anchor beyond the ±64 window still latched: %v", fs)
	}
	if !hasFinding(fs, "pii/ssn_bare") {
		t.Errorf("expected pii/ssn_bare finding, got %v", fs)
	}
}

func TestDetectSensitive_EmailBulk(t *testing.T) {
	five := "a1@example.com b2@example.com c3@example.com d4@example.com e5@example.com"
	fs := DetectSensitive(five)
	f, ok := findByName(fs, "pii/email_bulk")
	if !ok {
		t.Fatalf("expected pii/email_bulk finding, got %v", fs)
	}
	if f.Tier != TierWarn {
		t.Errorf("tier = %q, want %q", f.Tier, TierWarn)
	}
	if got := five[f.Start:f.End]; got != "a1@example.com" {
		t.Errorf("finding should span the first address, sliced %q", got)
	}

	four := "a1@example.com b2@example.com c3@example.com d4@example.com"
	if fs := DetectSensitive(four); hasFinding(fs, "pii/email_bulk") {
		t.Errorf("4 distinct addresses fired: %v", fs)
	}

	repeated := strings.Repeat("a1@example.com ", 9)
	if fs := DetectSensitive(repeated); hasFinding(fs, "pii/email_bulk") {
		t.Errorf("repeats of one address fired: %v", fs)
	}

	caseFolded := "A1@Example.com a1@example.COM b2@example.com c3@example.com d4@example.com"
	if fs := DetectSensitive(caseFolded); hasFinding(fs, "pii/email_bulk") {
		t.Errorf("case variants of one address counted as distinct: %v", fs)
	}
}

func TestDetectSensitive_SecretPattern(t *testing.T) {
	fs := DetectSensitive("key is sk-ant-" + "abc123_DEF-456")
	f, ok := findByName(fs, "secret/anthropic_api_key")
	if !ok {
		t.Fatalf("expected secret/anthropic_api_key finding, got %v", fs)
	}
	if f.Tier != TierLatch {
		t.Errorf("tier = %q, want %q", f.Tier, TierLatch)
	}
}

func TestDetectSensitive_ExampleKeyAllowlist(t *testing.T) {
	for _, key := range []string{"AKIAIOSFODNN7EXAMPLE", "AKIAI44QH8DHBEXAMPLE"} {
		if fs := DetectSensitive("old key was " + key + ", revoked"); len(fs) != 0 {
			t.Errorf("doc example %s produced findings: %v", key, fs)
		}
	}

	real := "AKIA" + "ABCDEFGHIJKLMNOP"
	fs := DetectSensitive("found " + real + " in .env")
	if !hasFinding(fs, "secret/aws_access_key_id") {
		t.Errorf("non-example AKIA key not detected: %v", fs)
	}
}

// Greedy variable-length patterns absorb an appended EXAMPLE into the
// match, so a within-match tail check let an attacker suppress
// detection of a real key by suffixing it. The exclusion is now
// exact-literal (plus the fixed-length AKIA tail convention), so
// these must keep latching.
func TestDetectSensitive_ExampleSuffixBypassPrevented(t *testing.T) {
	realGHP := "ghp_" + "myrealtoken1234"
	if fs := DetectSensitive("key: " + realGHP + "EXAMPLE"); !hasFinding(fs, "secret/github_pat") {
		t.Errorf("EXAMPLE-suffix bypass: %q not detected", realGHP+"EXAMPLE")
	}

	realAnthropic := "sk-ant-" + "abc123DEF456xyz789AB"
	if fs := DetectSensitive(realAnthropic + "EXAMPLE"); !hasFinding(fs, "secret/anthropic_api_key") {
		t.Errorf("EXAMPLE-suffix bypass: anthropic key %q not detected", realAnthropic+"EXAMPLE")
	}

	// Fixed-length AKIA cannot absorb the suffix: the appended EXAMPLE
	// sits after the 20-char match and must not suppress it.
	realAKIA := "AKIA" + "ABCDEFGHIJKLMNOP"
	if fs := DetectSensitive("leak " + realAKIA + "EXAMPLE"); !hasFinding(fs, "secret/aws_access_key_id") {
		t.Errorf("EXAMPLE-suffix bypass: AKIA key with appended EXAMPLE not detected: %v", fs)
	}
}

func TestDetectSensitive_SpanOffsets(t *testing.T) {
	key := "AKIA" + "ABCDEFGHIJKLMNOP"
	content := "before " + key + " after"
	fs := DetectSensitive(content)
	f, ok := findByName(fs, "secret/aws_access_key_id")
	if !ok {
		t.Fatalf("expected secret/aws_access_key_id finding, got %v", fs)
	}
	if got := content[f.Start:f.End]; got != key {
		t.Errorf("span sliced %q, want %q", got, key)
	}
}

// aws_secret_access_key has a (?i) prefix so its lowerRe is non-nil —
// this exercises the lowerRe scan path, whose offsets Wave 3 uses for
// redaction. Uppercase characters before the key pin the byte-
// preserving property of asciiLower: replacing it with anything that
// can change byte length would shift the span and fail here.
func TestDetectSensitive_SpanOffsets_LowerRePath(t *testing.T) {
	key := strings.Repeat("Ab1+", 10)
	content := "BEFORE AWS_SECRET_ACCESS_KEY = " + key + " AFTER"
	fs := DetectSensitive(content)
	f, ok := findByName(fs, "secret/aws_secret_access_key")
	if !ok {
		t.Fatalf("expected secret/aws_secret_access_key finding, got %v", fs)
	}
	got := content[f.Start:f.End]
	if !strings.HasSuffix(got, key) || !strings.HasPrefix(got, "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("lowerRe span sliced %q, want %q through %q", got, "AWS_SECRET_ACCESS_KEY", key)
	}
}

// Pins the deviation from the scrubber pack: a secret:// reference is
// a pointer, not key material, so it reports at warn tier.
func TestDetectSensitive_SecretRefIsWarn(t *testing.T) {
	fs := DetectSensitive(`"apiKeyRef": "secret://ANTHROPIC_API_KEY"`)
	f, ok := findByName(fs, "secret/secret_ref")
	if !ok {
		t.Fatalf("expected secret/secret_ref finding, got %v", fs)
	}
	if f.Tier != TierWarn {
		t.Errorf("tier = %q, want %q", f.Tier, TierWarn)
	}
}

func TestDetectSensitive_BearerProse(t *testing.T) {
	if fs := DetectSensitive("Use Bearer tokens for HTTP authentication"); hasFinding(fs, "secret/bearer_token") {
		t.Errorf("prose mention of bearer tokens latched: %v", fs)
	}

	fs := DetectSensitive("Authorization: Bearer " + "a1b2c3d4e5f6g7h8i9j0k1")
	if !hasFinding(fs, "secret/bearer_token") {
		t.Errorf("credential-shaped bearer token not detected: %v", fs)
	}
}

func TestDetectSensitive_BasicAuthProse(t *testing.T) {
	if fs := DetectSensitive("Basic auth is the simplest scheme"); hasFinding(fs, "secret/basic_auth") {
		t.Errorf("prose mention of basic auth latched: %v", fs)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("123456:" + "glc_token12345"))
	fs := DetectSensitive("Authorization: Basic " + payload)
	if !hasFinding(fs, "secret/basic_auth") {
		t.Errorf("real basic credential not detected: %v", fs)
	}
}

func TestDetectSensitive_GrafanaDocExampleAllowlisted(t *testing.T) {
	fs := DetectSensitive(`auth: "Basic MTIzNDU2OmdsY18uLi4="`)
	if hasFinding(fs, "secret/basic_auth") {
		t.Errorf("docs/observability-cloud.md example latched: %v", fs)
	}
}

func TestDetectSensitive_APIKeyHeaderSecretRef(t *testing.T) {
	for _, input := range []string{
		"dd-api-key: secret://DD_API_KEY",
		"x-api-key: secret://X_API_KEY",
	} {
		for _, f := range DetectSensitive(input) {
			if f.Tier == TierLatch {
				t.Errorf("input %q: secret:// header value produced latch finding %s", input, f.Name)
			}
		}
	}

	hex32 := strings.Repeat("0123456789abcdef", 2)
	fs := DetectSensitive("api-key: " + hex32)
	if !hasFinding(fs, "secret/api_key_header") {
		t.Errorf("real api-key header value not detected: %v", fs)
	}
}

// Suppressing any match followed by "://" let an attacker drop
// api_key_header detection by appending "://anything" to a real
// value. Only the bare secret://-reference shape may suppress.
func TestDetectSensitive_APIKeyHeaderURLSuffixBypassPrevented(t *testing.T) {
	realKey := strings.Repeat("0123456789abcdef", 2)
	fs := DetectSensitive("api-key: " + realKey + "://api.attacker.example/steal")
	if !hasFinding(fs, "secret/api_key_header") {
		t.Errorf("api-key value followed by :// was suppressed: %v", fs)
	}

	// A value that merely ends in the word "secret" is still potential
	// key material; only the exact scheme word is a reference.
	fs = DetectSensitive("api-key: " + "a1b2c3d4e5f6secret" + "://api.attacker.example/steal")
	if !hasFinding(fs, "secret/api_key_header") {
		t.Errorf("api-key value ending in 'secret' followed by :// was suppressed: %v", fs)
	}
}

func TestDetectSensitive_Empty(t *testing.T) {
	if fs := DetectSensitive(""); fs != nil {
		t.Errorf("empty content produced findings: %v", fs)
	}
}

// Mixed-case inputs exercise the ASCII-lowered fast path that replaces
// the canonical (?i) regexes; missing them here means the lowered
// variants drifted from the scrubber sources.
func TestDetectSensitive_CaseInsensitivePatternsViaLoweredPath(t *testing.T) {
	fs := DetectSensitive("Authorization: BEARER " + "A1B2C3D4E5F6G7H8I9J0")
	if !hasFinding(fs, "secret/bearer_token") {
		t.Errorf("uppercase bearer credential not detected: %v", fs)
	}

	val := strings.Repeat("Ab1+", 10)
	fs = DetectSensitive("AWS_SECRET_ACCESS_KEY = " + val)
	if !hasFinding(fs, "secret/aws_secret_access_key") {
		t.Errorf("uppercase aws_secret_access_key assignment not detected: %v", fs)
	}

	hex32 := strings.Repeat("0123456789ABCDEF", 2)
	fs = DetectSensitive("PASSWORD: " + hex32)
	if !hasFinding(fs, "secret/generic_hex_secret") {
		t.Errorf("uppercase generic hex secret not detected: %v", fs)
	}
}

// Every prefilter literal must keep appearing in its scrubber regex
// source: the prefilters skip the regex when no literal is present, so
// a scrubber rename that drops a literal would silently disable
// detection rather than fail loudly.
func TestPrefilterLiteralsStayInSyncWithScrubberSources(t *testing.T) {
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
		src, ok := sources[name]
		if !ok {
			t.Errorf("scrubber pattern %q no longer exists", name)
			continue
		}
		for _, lit := range lits {
			if !strings.Contains(src, lit) {
				t.Errorf("prefilter literal %q for %s not found in regex source %q", lit, name, src)
			}
		}
	}
}

// Every derived lowerRe must agree with its canonical (?i) regex —
// same matches, same byte offsets — over a corpus that actually
// exercises the six case-insensitive patterns in mixed case. A
// divergence means lowerVariant mis-lowered a construct.
func TestLowerVariant_EquivalentToCanonical(t *testing.T) {
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
	hasLowerRe := 0
	for _, p := range sensitivePatterns {
		if p.lowerRe == nil {
			continue
		}
		hasLowerRe++
		for _, s := range corpus {
			canonical := p.Re.FindAllStringIndex(s, -1)
			lowered := p.lowerRe.FindAllStringIndex(asciiLower(s), -1)
			if !reflect.DeepEqual(canonical, lowered) {
				t.Errorf("pattern %s diverges on %q: canonical %v, lowerRe %v", p.Name, s, canonical, lowered)
			}
		}
	}
	if hasLowerRe == 0 {
		t.Error("no pattern has a lowerRe — the fast path is dead and this test vacuous")
	}
}

func TestHasUnsafeClassRange(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`[0-F]`, true},
		{`[A-z]`, true},
		{`[?-Z]`, true},
		{`[Z-a]`, true},
		// Both-uppercase ranges lower cleanly and must stay eligible:
		// every current (?i) scrubber pattern contains [A-Za-z...].
		{`[A-Z]`, false},
		{`[A-Za-z0-9+/]`, false},
		{`[a-z0-9]`, false},
		{`\baws\b\s*[:=]\s*[A-Za-z0-9/+=]{40}`, false},
		{`abc-DEF`, false},
		{`[a\-Z]`, false},
	}
	for _, tc := range cases {
		if got := hasUnsafeClassRange(tc.src); got != tc.want {
			t.Errorf("hasUnsafeClassRange(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// A regex that can match the empty string must not pin detectBulk in
// place; emailRe cannot produce one today, but a future MinDistinct
// pattern with a *-quantifier would otherwise infinite-loop on every
// tool result.
func TestDetectBulk_ZeroLengthMatchTerminates(t *testing.T) {
	p := &SensitivePattern{Name: "test/zero", Re: regexp.MustCompile(`a*`), Tier: TierWarn, MinDistinct: 3}
	if f, ok := detectBulk("bbbb", p); ok {
		t.Errorf("only the empty string matches, expected no finding, got %v", f)
	}
	if _, ok := detectBulk("xaxbxaax", p); !ok {
		t.Error(`expected a finding: "", "a", "aa" are three distinct matches`)
	}
}

// The DetectSensitive memo caches scan results keyed on the p.Re
// pointer alone. Correctness requires that consecutive patterns
// sharing a *regexp.Regexp also agree on lowerRe (both nil or both the
// same), or one pattern would silently receive matches computed
// against the wrong haystack.
func TestSensitivePatternCacheInvariant(t *testing.T) {
	for i := 1; i < len(sensitivePatterns); i++ {
		a, b := sensitivePatterns[i-1], sensitivePatterns[i]
		if a.Re == b.Re && a.lowerRe != b.lowerRe {
			t.Errorf("patterns %s and %s share Re but differ on lowerRe — memo cache would serve wrong matches", a.Name, b.Name)
		}
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

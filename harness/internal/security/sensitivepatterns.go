package security

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"strings"
)

// Tier values for SensitivePattern and SensitiveFinding. A latch-tier
// finding is high-confidence sensitive data intended to trip the
// rule-of-two latch; a warn-tier finding is a lower-confidence signal
// surfaced in events without changing run posture.
const (
	TierLatch = "latch"
	TierWarn  = "warn"
)

// SensitivePattern is one rule in the sensitive-content pattern pack
// scanned by DetectSensitive.
type SensitivePattern struct {
	Name string
	Re   *regexp.Regexp
	Tier string
	// Validate, when non-nil, must return true for a regex match to
	// become a finding. It receives the full scanned content plus the
	// match span (the match is content[start:end]) so checksum
	// validators (Luhn, mod-97) and context-window checks (SSN
	// anchoring) share one signature.
	Validate func(content string, start, end int) bool
	// MinDistinct, when positive, makes the rule chunk-level: it fires
	// at most once per scanned content, and only when the number of
	// distinct (case-folded) matches reaches the threshold. The single
	// finding spans the first match.
	MinDistinct int

	// prefilter, when non-nil, is a cheap necessary-condition check on
	// the ASCII-lowered content; false skips the regex entirely.
	prefilter func(lower string) bool
	// lowerRe, when non-nil, is a case-sensitive equivalent of Re that
	// is run against the ASCII-lowered content instead. Lowering is
	// byte-preserving, so match offsets index the original content.
	lowerRe *regexp.Regexp
}

// SensitiveFinding is one detector hit. Start and End are byte offsets
// into the scanned content (content[Start:End] is the matched text) so
// callers can redact spans without re-running the regexes.
type SensitiveFinding struct {
	Name  string
	Tier  string
	Start int
	End   int
}

var (
	creditCardRe = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)
	ibanRe       = regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`)
	ssnRe        = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	emailRe      = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// sensitivePatterns is the pattern pack compiled at package init.
//
// Every LogScrubber secret pattern participates: config-referenced
// operational secrets (apiKeyRef and friends) are deliberately NOT
// rule-of-two sensitive data (see ruleOfTwoSensitiveData in
// types/runconfig.go), but key material observed in conversation
// content — an agent reading a .env, a token echoed in a tool
// result — is agent-readable sensitive data.
var sensitivePatterns = buildSensitivePatterns()

// Prefilter literal sets for the patterns that stay expensive even on
// the ASCII-lowered fast path (leading \b or alternation defeats the
// regexp engine's literal-prefix search). Each set is a necessary
// condition: a regex match always contains at least one of the
// literals, so absence proves the pattern cannot fire. The sync with
// the scrubber regex sources is pinned by a test.
var (
	awsSecretPrefilterLits    = []string{"aws_secret_access_key"}
	azureKeyPrefilterLits     = []string{"account_key", "accountkey", "access_key"}
	apiKeyHeaderPrefilterLits = []string{"api-key", "ocp-apim-subscription-key"}
	genericHexPrefilterLits   = []string{"key", "token", "secret", "password"}
)

func buildSensitivePatterns() []SensitivePattern {
	secrets := SecretPatterns()
	out := make([]SensitivePattern, 0, len(secrets)+5)
	for _, p := range secrets {
		sp := SensitivePattern{
			Name:     "secret/" + p.Name,
			Re:       p.Re,
			Tier:     TierLatch,
			Validate: notDocExample,
			lowerRe:  lowerVariant(p.Re.String()),
		}
		switch p.Name {
		case "secret_ref":
			// A secret:// reference is a pointer resolved through the
			// SecretStore, not key material — observing one does not
			// give the agent the secret, and ordinary documentation
			// (including this project's own docs) is full of them.
			// Worth an event, not a latch.
			sp.Tier = TierWarn
		case "bearer_token":
			// The scrubber regex also matches prose ("Bearer tokens
			// are..."), which is fine for log redaction but would
			// latch every conversation that discusses HTTP auth.
			sp.Validate = validBearerToken
		case "basic_auth":
			// Same prose problem ("Basic auth is...").
			sp.Validate = validBasicAuth
		case "api_key_header":
			sp.Validate = validAPIKeyHeader
			sp.prefilter = anyLiteral(apiKeyHeaderPrefilterLits)
		case "aws_access_key_id":
			sp.Validate = validAWSAccessKeyID
		case "aws_secret_access_key":
			sp.prefilter = anyLiteral(awsSecretPrefilterLits)
		case "azure_storage_key":
			sp.prefilter = anyLiteral(azureKeyPrefilterLits)
		case "generic_hex_secret":
			keyword := anyLiteral(genericHexPrefilterLits)
			sp.prefilter = func(lower string) bool {
				return keyword(lower) && hasHexRun(lower, 32)
			}
		}
		out = append(out, sp)
	}
	out = append(out,
		SensitivePattern{Name: "pii/credit_card", Re: creditCardRe, Tier: TierLatch, Validate: validLuhn},
		SensitivePattern{Name: "pii/iban", Re: ibanRe, Tier: TierLatch, Validate: validIBAN},
		// The two SSN rules share one regex (DetectSensitive scans it
		// once) and split on the context anchor, so exactly one of
		// them fires per match and an anchored SSN does not also
		// produce a duplicate warn finding.
		SensitivePattern{Name: "pii/ssn_anchored", Re: ssnRe, Tier: TierLatch, Validate: ssnAnchorNearby},
		SensitivePattern{Name: "pii/ssn_bare", Re: ssnRe, Tier: TierWarn, Validate: ssnAnchorAbsent},
		// PEM private keys are already covered by secret/pem_private_key.
		SensitivePattern{Name: "pii/email_bulk", Re: emailRe, Tier: TierWarn, MinDistinct: 5,
			prefilter: func(lower string) bool { return strings.IndexByte(lower, '@') >= 0 }},
	)
	return out
}

// lowerVariant derives a case-sensitive equivalent of a (?i)-prefixed
// pattern for matching against ASCII-lowered content. ASCII lowering
// preserves byte offsets and the membership of every escaped class
// (\b \s \w \d and their negations are unaffected when A-Z map to
// a-z), so only unescaped literals and class ranges need lowering.
// Returns nil — callers fall back to the canonical regex on the
// original content — when the pattern is not (?i)-prefixed or uses an
// escape (\xNN, \p{...}, octal) whose mechanical lowering could change
// semantics. Folding is ASCII-only: exotic Unicode case variants
// (U+017F long s, U+212A Kelvin) are not folded, which is acceptable
// for credential formats that are ASCII by construction.
func lowerVariant(src string) *regexp.Regexp {
	rest, ok := strings.CutPrefix(src, "(?i)")
	if !ok {
		return nil
	}
	if hasUnsafeClassRange(rest) {
		return nil
	}
	var b strings.Builder
	b.Grow(len(rest))
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c == '\\' && i+1 < len(rest) {
			n := rest[i+1]
			if n == 'x' || n == 'X' || n == 'p' || n == 'P' || (n >= '0' && n <= '9') {
				return nil
			}
			b.WriteByte(c)
			b.WriteByte(n)
			i++
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// hasUnsafeClassRange reports whether the pattern fragment contains a
// character-class range with exactly one uppercase-letter endpoint,
// e.g. [0-F] or [A-z]. Mechanically lowering such a range changes its
// semantics: [0-F] → [0-f] admits punctuation and a-f that the
// original never matched. Ranges with both endpoints uppercase ([A-Z])
// lower cleanly — ASCII lowering is an order-preserving bijection from
// A-Z to a-z — and are left for lowerVariant to rewrite; bailing on
// them too would disqualify every current (?i) scrubber pattern (they
// all contain [A-Za-z...]) and forfeit the fast path entirely. A
// leading/trailing '-' inside a class is a literal, not a range.
func hasUnsafeClassRange(src string) bool {
	inClass := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\\' {
			i++
			continue
		}
		switch c {
		case '[':
			inClass = true
			continue
		case ']':
			inClass = false
			continue
		}
		if !inClass || c != '-' || i == 0 || i+1 >= len(src) {
			continue
		}
		lo, hi := src[i-1], src[i+1]
		if lo == '[' || hi == ']' {
			continue
		}
		loUpper := lo >= 'A' && lo <= 'Z'
		hiUpper := hi >= 'A' && hi <= 'Z'
		if loUpper != hiUpper {
			return true
		}
	}
	return false
}

func asciiLower(s string) string {
	i := 0
	for ; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			break
		}
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func anyLiteral(lits []string) func(string) bool {
	return func(lower string) bool {
		for _, l := range lits {
			if strings.Contains(lower, l) {
				return true
			}
		}
		return false
	}
}

func hasHexRun(lower string, n int) bool {
	run := 0
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			run++
			if run >= n {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// SensitivePatterns returns a copy of the sensitive-content pattern
// pack. The slice is freshly allocated; the regexp values are shared
// (regexp.Regexp is safe for concurrent use).
func SensitivePatterns() []SensitivePattern {
	out := make([]SensitivePattern, len(sensitivePatterns))
	copy(out, sensitivePatterns)
	return out
}

// DetectSensitive scans content against the full pattern pack and
// returns findings in (pattern-order, byte-offset-order). It runs on
// every tool result, so the no-finding path allocates nothing beyond
// one ASCII-lowered copy of mixed-case content (which feeds the
// prefilters and the case-sensitive variants of the (?i) patterns)
// and regexp's pooled machine state.
func DetectSensitive(content string) []SensitiveFinding {
	if content == "" {
		return nil
	}
	lower := asciiLower(content)
	var findings []SensitiveFinding
	var lastRe *regexp.Regexp
	var lastMatches [][]int
	for i := range sensitivePatterns {
		p := &sensitivePatterns[i]
		if p.prefilter != nil && !p.prefilter(lower) {
			continue
		}
		if p.MinDistinct > 0 {
			if f, ok := detectBulk(content, p); ok {
				findings = append(findings, f)
			}
			continue
		}
		var matches [][]int
		if p.Re == lastRe {
			matches = lastMatches
		} else {
			re, haystack := p.Re, content
			if p.lowerRe != nil {
				re, haystack = p.lowerRe, lower
			}
			matches = re.FindAllStringIndex(haystack, -1)
			lastRe, lastMatches = p.Re, matches
		}
		for _, m := range matches {
			if p.Validate != nil && !p.Validate(content, m[0], m[1]) {
				continue
			}
			findings = append(findings, SensitiveFinding{Name: p.Name, Tier: p.Tier, Start: m[0], End: m[1]})
		}
	}
	return findings
}

// detectBulk evaluates a chunk-level rule incrementally so content
// dense with repeats of the same match (a log full of one email
// address) never materializes the full match list. The distinct count
// is case-folded; the returned finding spans the first match.
func detectBulk(content string, p *SensitivePattern) (SensitiveFinding, bool) {
	var seen map[string]struct{}
	first, firstEnd := -1, 0
	pos := 0
	for pos < len(content) {
		loc := p.Re.FindStringIndex(content[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		if first < 0 {
			first, firstEnd = start, end
		}
		if seen == nil {
			seen = make(map[string]struct{}, p.MinDistinct)
		}
		seen[strings.ToLower(content[start:end])] = struct{}{}
		if len(seen) >= p.MinDistinct {
			return SensitiveFinding{Name: p.Name, Tier: p.Tier, Start: first, End: firstEnd}, true
		}
		pos = end
		if loc[1] == loc[0] {
			// A zero-length match would pin pos in place; emailRe
			// cannot produce one, but a future MinDistinct pattern
			// with a *-quantified regex must not infinite-loop.
			pos++
		}
	}
	return SensitiveFinding{}, false
}

// docExampleLiterals lists canonical documentation placeholder
// credentials that must never produce findings. The Grafana Cloud
// Basic-auth string decodes to "123456:glc_..." and appears verbatim
// in docs/observability-cloud.md.
var docExampleLiterals = []string{
	"MTIzNDU2OmdsY18uLi4=",
}

// notDocExample rejects canonical documentation examples by exact
// literal only. It deliberately does NOT exclude matches that merely
// end in "EXAMPLE": the secret patterns have greedy variable-length
// tails, so an appended EXAMPLE is absorbed into the match and a
// tail check would let an attacker suppress detection of a real key
// by suffixing it (wave-2 review bypass). The EXAMPLE-tail
// convention is handled per-pattern where it is sound — see
// validAWSAccessKeyID.
func notDocExample(content string, start, end int) bool {
	m := content[start:end]
	for _, lit := range docExampleLiterals {
		if strings.Contains(m, lit) {
			return false
		}
	}
	return true
}

// validAWSAccessKeyID adds the EXAMPLE-tail convention on top of the
// literal allowlist: AWS reserves key IDs ending in EXAMPLE for
// documentation (AKIAIOSFODNN7EXAMPLE and friends, which appear in
// this repo's eval suites). The check is sound here and only here
// because the pattern is fixed-length: an EXAMPLE appended to a real
// key lands after the 20-char match — the regex cannot absorb it —
// so the match tail, and detection, are unchanged. Variable-length
// patterns must keep using notDocExample alone; their greedy classes
// absorb the suffix into the match, which is exactly the wave-2
// review bypass.
func validAWSAccessKeyID(content string, start, end int) bool {
	if !notDocExample(content, start, end) {
		return false
	}
	return !strings.HasSuffix(content[start:end], "EXAMPLE")
}

// tokenAfterSpace returns the substring after the last whitespace in
// m — the credential part of a "Scheme <credential>" match.
func tokenAfterSpace(m string) string {
	if i := strings.LastIndexAny(m, " \t\r\n\v\f"); i >= 0 {
		return m[i+1:]
	}
	return m
}

// validBearerToken keeps credential-shaped bearer values and drops
// prose. Real-world bearer credentials (JWTs, PATs, OAuth tokens) are
// long and carry digits or punctuation; a purely alphabetic token of
// 16+ characters is indistinguishable from an English word and is
// deliberately missed.
func validBearerToken(content string, start, end int) bool {
	if !notDocExample(content, start, end) {
		return false
	}
	tok := tokenAfterSpace(content[start:end])
	return len(tok) >= 16 && !isASCIIAlpha(tok)
}

func isASCIIAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// validBasicAuth requires the payload to decode as base64 containing a
// "user:password" colon, dropping prose like "Basic auth" that the
// scrubber regex happily matches.
func validBasicAuth(content string, start, end int) bool {
	if !notDocExample(content, start, end) {
		return false
	}
	payload := tokenAfterSpace(content[start:end])
	if len(payload) < 16 {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if decoded, err = base64.RawStdEncoding.DecodeString(payload); err != nil {
			return false
		}
	}
	return bytes.IndexByte(decoded, ':') >= 0
}

// validAPIKeyHeader drops the secret://-reference shape and nothing
// else: `dd-api-key: secret://DD_API_KEY` matches the scrubber regex
// as `api-key: secret` because the value class excludes ':', leaving
// "://" immediately after the match. The value must be exactly the
// scheme word "secret" — suppressing on a bare "://" suffix would let
// an attacker drop detection of any real key by appending
// "://anything" to it (wave-2 review bypass), and a value that merely
// ends in "secret" could still be key material.
func validAPIKeyHeader(content string, start, end int) bool {
	if !notDocExample(content, start, end) {
		return false
	}
	m := content[start:end]
	if i := strings.IndexByte(m, ':'); i >= 0 {
		val := strings.TrimLeft(m[i+1:], " \t\r\n\v\f")
		if val == "secret" && strings.HasPrefix(content[end:], "://") {
			return false
		}
	}
	return true
}

// validLuhn strips separators and applies the Luhn checksum over the
// matched digit sequence. Canonical test PANs (4111111111111111 and
// friends) pass by design: the detector cannot distinguish a
// documented test number from a real card, and latching on test
// fixtures is the documented operator footgun (remediation:
// classifier "none" or an upfront sensitiveData declaration).
//
// Sequences starting with 0 are rejected: ISO/IEC 7812 reserves major
// industry identifier 0, so no issued card starts with it — and Luhn
// alone accepts all-zero sequences (sum 0), which would latch on nil
// UUIDs and zero-filled placeholder IDs.
func validLuhn(content string, start, end int) bool {
	if content[start] == '0' {
		return false
	}
	sum, n := 0, 0
	double := false
	for i := end - 1; i >= start; i-- {
		c := content[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
		n++
	}
	return n >= 13 && n <= 19 && sum%10 == 0
}

// validIBAN applies the ISO 13616 mod-97 check: move the leading
// country+check block to the end, map A-Z to 10-35, and require the
// resulting number ≡ 1 (mod 97).
func validIBAN(content string, start, end int) bool {
	m := content[start:end]
	rem := 0
	step := func(c byte) bool {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
		return true
	}
	for i := 4; i < len(m); i++ {
		if !step(m[i]) {
			return false
		}
	}
	for i := range 4 {
		if !step(m[i]) {
			return false
		}
	}
	return rem == 1
}

// ssnContextWindow is the byte distance around an SSN-shaped match in
// which an "ssn" / "social security" mention promotes the finding
// from warn to latch tier.
const ssnContextWindow = 64

func ssnAnchorNearby(content string, start, end int) bool {
	lo := max(start-ssnContextWindow, 0)
	hi := min(end+ssnContextWindow, len(content))
	window := strings.ToLower(content[lo:hi])
	return strings.Contains(window, "ssn") || strings.Contains(window, "social security")
}

func ssnAnchorAbsent(content string, start, end int) bool {
	return !ssnAnchorNearby(content, start, end)
}

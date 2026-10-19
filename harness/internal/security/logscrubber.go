package security

import (
	"regexp"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

// namedPattern pairs a regexp with a stable label used in ScrubStats.
type namedPattern struct {
	name string
	re   *regexp.Regexp
}

// secretPatterns defines the set of patterns that should be redacted from logs
// and other output. Each pattern matches a known secret or credential format.
// Names are stable identifiers safe to log (they reveal pattern type, not the
// matched secret value).
var secretPatterns = []namedPattern{
	// anthropic_wif_token MUST precede anthropic_api_key: both match
	// `sk-ant-oat01-...`, and only the order decides which label lands in
	// audit stats (the redaction is identical either way).
	//
	// The \b before each sk- prefix is load-bearing. Unanchored, the pattern
	// matches the tail of any identifier ending in "...sk-", and because
	// sensitivepatterns.go lifts these regexes into the Rule-of-Two detector
	// at TierLatch, a tool error echoing such a path revoked run_command
	// mid-run. No real key is lost: an API key is always preceded by
	// start-of-string or a non-word delimiter.
	{"anthropic_wif_token", regexp.MustCompile(`\bsk-ant-oat01-[a-zA-Z0-9_-]+`)},
	{"anthropic_api_key", regexp.MustCompile(`\bsk-ant-[a-zA-Z0-9_-]+`)},
	{"openai_api_key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)},
	{"stripe_live_key", regexp.MustCompile(`sk_live_[A-Za-z0-9]{16,}`)},
	{"github_pat", regexp.MustCompile(`ghp_[a-zA-Z0-9]+`)},
	{"github_app_token", regexp.MustCompile(`ghs_[a-zA-Z0-9]+`)},
	{"aws_access_key_id", regexp.MustCompile(`AKIA[A-Z0-9]{16}`)},
	// AWS secret access keys and Azure storage keys have high-entropy
	// base64-ish shapes but no distinctive prefix. Keep them anchored to
	// well-known key/value field names so ordinary hashes, IDs, and trace
	// fragments are not scrubbed just because they share an alphabet.
	{"aws_secret_access_key", regexp.MustCompile(`(?i)\baws_secret_access_key\b` + kvSep + `["']?[A-Za-z0-9/+=]{40}["']?`)},
	{"azure_storage_key", regexp.MustCompile(`(?i)\b(?:azure_storage_account_key|storage_account_key|account_key|accountKey|primary_access_key|secondary_access_key)\b` + kvSep + `["']?[A-Za-z0-9+/]{40,88}={0,2}["']?`)},
	{"slack_token", regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"gcp_api_key", regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	// basic_auth precedes bearer_token so the two HTTP-auth families
	// scrub independently. Grafana Cloud's documented credential
	// format is `Basic <base64(instanceID:apiToken)>` (see
	// docs/observability-cloud.md), and the bearer_token pattern below
	// would not match this prefix. Without this entry, a resolved
	// Basic token leaking into slog output would land unscrubbed.
	{"basic_auth", regexp.MustCompile(`(?i)Basic[ \t]+[A-Za-z0-9+/]+=*`)},
	{"bearer_token", regexp.MustCompile(`(?i)Bearer[ \t]+[A-Za-z0-9._~+/=-]+`)},
	{"pem_private_key", regexp.MustCompile(pemPrivateKeyPattern)},
	// Case-insensitive: a shell resolves "secret://" and "SECRET://"
	// identically, and this pattern is the last line of defence for a
	// hook command whose case-insensitive ValidateRunConfig rejection
	// was bypassed (e.g. RunConfig built without going through
	// validation) — matching lowercase only would let a case-varied
	// reference ride into a persisted trace unredacted. The body is the
	// reference syntax the secret stores accept (an environment variable,
	// file:// path or ssm:// parameter), so shell syntax after a
	// reference is never hidden from a guard.
	{"secret_ref", regexp.MustCompile(`(?i)secret://[A-Za-z0-9_./:@+~%=-]+`)},
	// api_key_header matches the literal "<header>: <value>" forms used
	// for non-Bearer auth on Azure OpenAI and APIM-fronted gateways. These
	// keys do not have a distinctive prefix (Azure keys are hex-y but
	// indistinguishable from arbitrary strings), so the header name is the
	// only reliable anchor. The pattern is anchored on a header name we
	// know stirrup emits — adding more variants here is preferable to a
	// permissive token catch-all.
	{"api_key_header", regexp.MustCompile(`(?i)\b(?:api-key|x-api-key|Ocp-Apim-Subscription-Key)[ \t]*:[ \t]*[A-Za-z0-9._~+/=-]+`)},
	{"generic_hex_secret", regexp.MustCompile(`(?i)\b(?:api[_-]?key|token|secret|client[_-]?secret|access[_-]?token|auth[_-]?token|password)\b` + kvSep + `["']?[a-f0-9]{32}["']?`)},
	// oidc_jwt matches a three-segment JWT whose header begins with
	// "eyJ" (the base64url prefix of `{"`). Federation flows propagate
	// runner OIDC tokens, AWS web-identity tokens, and GCP STS subject
	// tokens through error strings (truncateForError, GHA OIDC error
	// body, Azure IMDS error body) verbatim — without this entry a
	// hostile STS endpoint that echoes the subject token back in its
	// error body would produce an unscrubbed JWT in slog output and
	// OTel span status strings.
	{"oidc_jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*`)},
	// gcp_access_token matches Google OAuth2 access tokens, which all
	// share the documented "ya29." prefix. The body is the standard
	// base64url alphabet; the regex stops at any character outside
	// that set so trailing punctuation, whitespace, or quotes do not
	// get pulled into the redaction span.
	{"gcp_access_token", regexp.MustCompile(`ya29\.[A-Za-z0-9_-]+`)},
}

// kvSep separates a key from its value in the key/value patterns: a colon
// or equals sign, with the value on the same line or, as YAML allows, on
// the next line indented. An unindented next line is left alone, since it
// can be a command rather than a value.
const kvSep = `[ \t]*[:=][ \t]*(?:\r?\n[ \t]+)?`

// pemPrivateKeyPattern matches a PEM or PGP key block from its BEGIN line
// through its END line, or without one through the whole base64 lines after
// it. Lines may be raw, "\n"- or "\\n"-escaped, quoted list elements,
// concatenated string literals, or line-numbered with an optional path; the
// body admits only base64 lines, armour headers directly after BEGIN and
// the harness's own truncation marker, so a line of prose ends the match.
const pemPrivateKeyPattern = pemHeader + `(?:` +
	`(?:` + pemArmour + pemSep + pemItem + `)?(?:` + pemSep + pemItem + `)*` + pemSep + pemEnd + `|` +
	`(?:` + pemArmour + `(?:` + pemOpenLine + `))?(?:` + pemOpenLine + `)*)`

const (
	pemHeader    = `-----BEGIN[\s\w]+KEY(?:\s+BLOCK)?-----`
	pemEnd       = `-----END[\s\w]+KEY(?:\s+BLOCK)?-----`
	pemHSpace    = `[\t \x{A0}]*`
	pemEscBreak  = `\\\\r\\\\n|\\\\n|\\r\\n|\\n|"\s*,\s*"|'\s*,\s*'|"\s*\+\s*"|'\s*\+\s*'`
	pemLineBreak = `(?:\r?\n|` + pemEscBreak + `)`
	pemSep       = pemHSpace + `(?:` + pemLineBreak + pemHSpace + pemLineNumber + `)+`
	// pemLineNumber is the "12\t" of read_file and cat -n output, or the
	// "12:" of grep -n, before a line, after the "path:" of grep -rn and
	// search_files output.
	pemLineNumber = `(?:(?:[A-Za-z0-9_./~+@%=-]+:)?[0-9]+(?:\t|\\t|:)` + pemHSpace + `)?`
	pemItem       = `(?:[A-Za-z0-9+/=]+|\[\.\.\.truncated(?: [0-9]+ bytes)?\.\.\.\])`
	// pemArmour is RFC 1421/4880 armour header lines and the line break
	// that, with the next line's, makes the blank line before the body.
	pemArmour = `(?:` + pemSep + `[A-Za-z][A-Za-z0-9-]*:[^\r\n"\\;|&$` + "`" + `()<>]*)+` + pemHSpace + pemLineBreak
	// pemOpenLine is a body line of a block with no END line. A raw line
	// must be base64 to its end; an escaped one ends where base64 does.
	pemOpenLine = `(?:` + pemHSpace + `\r?\n` + pemHSpace + pemLineNumber + `)+` + pemItem + pemHSpace + `\r?(?m:$)|` +
		pemHSpace + `(?:(?:` + pemEscBreak + `)` + pemHSpace + `)+` + pemItem
)

// matchValidators narrows patterns whose regex also matches ordinary prose.
// A match is redacted only when its validator returns true; patterns absent
// from the map redact every match.
var matchValidators = map[string]func(match string) bool{
	"basic_auth":   isBasicCredential,
	"bearer_token": isBearerCredential,
}

// bearerProseWords are the words that follow "bearer" in prose about HTTP
// authentication rather than in a header carrying a token.
var bearerProseWords = map[string]bool{
	"token": true, "tokens": true, "auth": true, "authentication": true,
	"authorization": true, "scheme": true, "schemes": true, "header": true,
	"headers": true, "credential": true, "credentials": true, "value": true,
	"values": true, "string": true, "prefix": true, "type": true,
	"format": true, "and": true, "or": true, "is": true, "the": true,
	"a": true, "an": true,
}

// isBearerCredential reports whether a bearer_token match is a credential
// rather than prose such as "bearer token": every value is redacted except
// one of bearerProseWords.
func isBearerCredential(match string) bool {
	return !bearerProseWords[strings.ToLower(tokenAfterSpace(match))]
}

// isBasicCredential reports whether a basic_auth match is a credential
// rather than prose such as "basic documentation": the payload must decode
// from base64 to bytes containing the "user:password" colon.
func isBasicCredential(match string) bool {
	_, ok := decodeBasicPayload(tokenAfterSpace(match))
	return ok
}

// replaceValidated replaces every match of re that valid accepts.
func replaceValidated(re *regexp.Regexp, value string, valid func(string) bool) (string, int) {
	matches := findValidated(re, value, valid)
	if len(matches) == 0 {
		return value, 0
	}
	var b strings.Builder
	prev := 0
	for _, m := range matches {
		b.WriteString(value[prev:m[0]])
		b.WriteString(redactedPlaceholder)
		prev = m[1]
	}
	b.WriteString(value[prev:])
	return b.String(), len(matches)
}

// findValidated returns the matches of re that valid accepts. A rejected
// match does not consume the text after its first byte, so a credential
// directly following a rejected match is still examined.
func findValidated(re *regexp.Regexp, value string, valid func(string) bool) [][]int {
	var matches [][]int
	for off := 0; ; {
		loc := re.FindStringIndex(value[off:])
		if loc == nil {
			return matches
		}
		start, end := off+loc[0], off+loc[1]
		if valid(value[start:end]) {
			matches = append(matches, []int{start, end})
			off = end
			continue
		}
		off = start + 1
	}
}

// ScrubStats reports redactions performed by ScrubWithStats. Count is the
// total number of replacements across all patterns; Patterns is the
// deduplicated list of pattern names that matched at least once.
type ScrubStats struct {
	Count    int
	Patterns []string
}

// SecretPattern is a read-only view of a single secret-detection pattern,
// exposed so other packages (e.g. codescanner) can reuse the canonical
// regex set without duplicating the definitions.
type SecretPattern struct {
	Name string
	Re   *regexp.Regexp
}

// SecretPatterns returns a copy of the canonical secret-detection regex set
// used by Scrub. The returned slice is freshly allocated; the regexp values
// are shared (regexp.Regexp is safe for concurrent use).
func SecretPatterns() []SecretPattern {
	out := make([]SecretPattern, len(secretPatterns))
	for i, p := range secretPatterns {
		out[i] = SecretPattern{Name: p.name, Re: p.re}
	}
	return out
}

// Scrub replaces all known secret patterns in value with "[REDACTED]".
// Equivalent to ScrubWithStats with the stats discarded.
func Scrub(value string) string {
	scrubbed, _ := ScrubWithStats(value)
	return scrubbed
}

// ScrubWithStats performs the same replacement as Scrub but additionally
// reports how many replacements occurred and which pattern names matched.
// Pattern names are stable identifiers (e.g. "anthropic_api_key") suitable
// for logging — they describe the pattern type only, never the matched
// secret value.
func ScrubWithStats(value string) (string, ScrubStats) {
	stats := ScrubStats{}
	for _, p := range secretPatterns {
		valid := matchValidators[p.name]
		if valid == nil {
			matches := p.re.FindAllStringIndex(value, -1)
			if len(matches) == 0 {
				continue
			}
			stats.Count += len(matches)
			stats.Patterns = append(stats.Patterns, p.name)
			value = p.re.ReplaceAllString(value, redactedPlaceholder)
			continue
		}
		var redacted int
		value, redacted = replaceValidated(p.re, value, valid)
		if redacted > 0 {
			stats.Count += redacted
			stats.Patterns = append(stats.Patterns, p.name)
		}
	}
	return value, stats
}

// ScrubCorrelation returns a copy of c with every value scrubbed. Label
// keys are left as-is: their pattern cannot spell a known secret format.
func ScrubCorrelation(c *types.CorrelationConfig) *types.CorrelationConfig {
	if c == nil {
		return nil
	}
	out := &types.CorrelationConfig{
		SessionID: Scrub(c.SessionID),
		TaskID:    Scrub(c.TaskID),
		AttemptID: Scrub(c.AttemptID),
		UserID:    Scrub(c.UserID),
		URL:       Scrub(c.URL),
	}
	if c.Labels != nil {
		out.Labels = make(map[string]string, len(c.Labels))
		for k, v := range c.Labels {
			out.Labels[k] = Scrub(v)
		}
	}
	return out
}

// ScrubMap returns a deep copy of data with all string values scrubbed.
// Non-string leaf values are copied as-is; nested maps and slices are
// traversed recursively.
func ScrubMap(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = scrubValue(v)
	}
	return out
}

func scrubValue(v any) any {
	switch val := v.(type) {
	case string:
		return Scrub(val)
	case map[string]any:
		return ScrubMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = scrubValue(item)
		}
		return out
	default:
		return v
	}
}

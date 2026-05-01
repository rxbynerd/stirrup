package security

import "regexp"

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
	{"anthropic_api_key", regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]+`)},
	{"openai_api_key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)},
	{"github_pat", regexp.MustCompile(`ghp_[a-zA-Z0-9]+`)},
	{"github_app_token", regexp.MustCompile(`ghs_[a-zA-Z0-9]+`)},
	{"aws_access_key_id", regexp.MustCompile(`AKIA[A-Z0-9]{16}`)},
	{"bearer_token", regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`)},
	{"pem_private_key", regexp.MustCompile(`-----BEGIN[\s\w]+KEY-----`)},
	{"secret_ref", regexp.MustCompile(`secret://[^\s"']+`)},
	// api_key_header matches the literal "<header>: <value>" forms used
	// for non-Bearer auth on Azure OpenAI and APIM-fronted gateways. These
	// keys do not have a distinctive prefix (Azure keys are hex-y but
	// indistinguishable from arbitrary strings), so the header name is the
	// only reliable anchor. The pattern is anchored on a header name we
	// know stirrup emits — adding more variants here is preferable to a
	// permissive token catch-all.
	{"api_key_header", regexp.MustCompile(`(?i)\b(?:api-key|x-api-key|Ocp-Apim-Subscription-Key)\s*:\s*[A-Za-z0-9._~+/=-]+`)},
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
		matches := p.re.FindAllStringIndex(value, -1)
		if len(matches) == 0 {
			continue
		}
		stats.Count += len(matches)
		stats.Patterns = append(stats.Patterns, p.name)
		value = p.re.ReplaceAllString(value, redactedPlaceholder)
	}
	return value, stats
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

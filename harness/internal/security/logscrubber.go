package security

import "regexp"

// secretPatterns defines the set of patterns that should be redacted from logs
// and other output. Each pattern matches a known secret or credential format.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]+`),      // Anthropic API keys
	regexp.MustCompile(`ghp_[a-zA-Z0-9]+`),            // GitHub PATs
	regexp.MustCompile(`ghs_[a-zA-Z0-9]+`),            // GitHub app tokens
	regexp.MustCompile(`AKIA[A-Z0-9]{16}`),            // AWS access key IDs
	regexp.MustCompile(`Bearer\s+[a-zA-Z0-9._-]+`),   // Bearer tokens
	regexp.MustCompile(`-----BEGIN[\s\w]+KEY-----`), // PEM private keys
	regexp.MustCompile(`secret://[^\s"']+`),           // Secret store references
}

// Scrub replaces all known secret patterns in value with "[REDACTED]".
func Scrub(value string) string {
	for _, p := range secretPatterns {
		value = p.ReplaceAllString(value, redactedPlaceholder)
	}
	return value
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

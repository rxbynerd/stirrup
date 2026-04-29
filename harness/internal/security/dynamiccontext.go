package security

import "regexp"

const maxDynamicContextValueLength = 50_000

var xmlHTMLTagPattern = regexp.MustCompile(`(?s)<!--.*?-->|<\?[^>]*\?>|<![^>]*>|</?[A-Za-z][A-Za-z0-9:_-]*(?:\s+[^<>]*)?>`)

// DynamicContextSanitizationEvent describes a dynamic-context value changed by
// sanitization. It intentionally omits the value content.
type DynamicContextSanitizationEvent struct {
	Key             string   `json:"key"`
	OriginalLength  int      `json:"originalLength"`
	SanitizedLength int      `json:"sanitizedLength"`
	Reasons         []string `json:"reasons"`
}

// SanitizeDynamicContext strips delimiter-like markup and caps each value so
// external context cannot mimic trusted prompt structure.
func SanitizeDynamicContext(input map[string]string) (map[string]string, []DynamicContextSanitizationEvent) {
	if input == nil {
		return nil, nil
	}

	out := make(map[string]string, len(input))
	var events []DynamicContextSanitizationEvent
	for k, v := range input {
		original := v
		reasons := make([]string, 0, 2)

		sanitized := xmlHTMLTagPattern.ReplaceAllString(v, "")
		if sanitized != v {
			reasons = append(reasons, "tags_stripped")
		}
		if len(sanitized) > maxDynamicContextValueLength {
			sanitized = sanitized[:maxDynamicContextValueLength]
			reasons = append(reasons, "truncated")
		}

		out[k] = sanitized
		if len(reasons) > 0 {
			events = append(events, DynamicContextSanitizationEvent{
				Key:             k,
				OriginalLength:  len(original),
				SanitizedLength: len(sanitized),
				Reasons:         reasons,
			})
		}
	}
	return out, events
}

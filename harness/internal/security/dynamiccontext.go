package security

import "regexp"

// MaxOperatorTextBytes caps every control-plane-supplied free-text value
// that reaches the model context — dynamicContext values and mid-run
// user_response input alike. The two share a trust tier, so they share
// the bound and the markup stripping below.
const MaxOperatorTextBytes = 50_000

var xmlHTMLTagPattern = regexp.MustCompile(`(?s)<!--.*?-->|<\?[^>]*\?>|<![^>]*>|</?[A-Za-z][A-Za-z0-9:_-]*(?:\s+[^<>]*)?>`)

// DynamicContextSanitizationEvent describes a dynamic-context value changed by
// sanitization. It intentionally omits the value content.
type DynamicContextSanitizationEvent struct {
	Key             string   `json:"key"`
	OriginalLength  int      `json:"originalLength"`
	SanitizedLength int      `json:"sanitizedLength"`
	Reasons         []string `json:"reasons"`
}

// SanitizeOperatorText strips delimiter-like markup from v and caps it
// at MaxOperatorTextBytes, returning the result and the reasons it
// changed ("tags_stripped", "truncated"); nil reasons means v was
// returned unchanged.
func SanitizeOperatorText(v string) (string, []string) {
	var reasons []string
	sanitized := xmlHTMLTagPattern.ReplaceAllString(v, "")
	if sanitized != v {
		reasons = append(reasons, "tags_stripped")
	}
	if len(sanitized) > MaxOperatorTextBytes {
		sanitized = sanitized[:MaxOperatorTextBytes]
		reasons = append(reasons, "truncated")
	}
	return sanitized, reasons
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
		sanitized, reasons := SanitizeOperatorText(v)
		out[k] = sanitized
		if len(reasons) > 0 {
			events = append(events, DynamicContextSanitizationEvent{
				Key:             k,
				OriginalLength:  len(v),
				SanitizedLength: len(sanitized),
				Reasons:         reasons,
			})
		}
	}
	return out, events
}

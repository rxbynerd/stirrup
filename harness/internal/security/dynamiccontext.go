package security

import (
	"regexp"
	"strings"
)

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
// returned unchanged. Stripping repeats until no markup remains, so
// removing an inner tag cannot join its neighbours into a new tag.
func SanitizeOperatorText(v string) (string, []string) {
	var reasons []string
	sanitized := stripMarkup(v)
	if sanitized != v {
		reasons = append(reasons, "tags_stripped")
	}
	if len(sanitized) > MaxOperatorTextBytes {
		sanitized = sanitized[:MaxOperatorTextBytes]
		reasons = append(reasons, "truncated")
	}
	return sanitized, reasons
}

// maxMarkupStripPasses bounds stripMarkup's rescans. Each pass only peels
// one layer of nesting, so deliberately deep nesting would otherwise cost
// time quadratic in the input length.
const maxMarkupStripPasses = 16

// stripMarkup removes markup until none remains. Input still holding
// markup after maxMarkupStripPasses has every remaining '<' removed, which
// leaves no tag of any kind.
func stripMarkup(v string) string {
	for range maxMarkupStripPasses {
		next := xmlHTMLTagPattern.ReplaceAllString(v, "")
		if next == v {
			return v
		}
		v = next
	}
	if xmlHTMLTagPattern.MatchString(v) {
		v = strings.ReplaceAll(v, "<", "")
	}
	return v
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

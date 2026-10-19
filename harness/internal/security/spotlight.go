package security

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// Spotlight fence tags. A fence holds the standard base64 encoding of the
// content it stands for.
const (
	SpotlightOpenTag  = "<untrusted_content_b64>"
	SpotlightCloseTag = "</untrusted_content_b64>"
)

// Spotlight returns content base64-encoded inside the spotlight tags.
func Spotlight(content string) string {
	return SpotlightOpenTag + base64.StdEncoding.EncodeToString([]byte(content)) + SpotlightCloseTag
}

// DecodeFences returns s with each spotlight fence replaced by the text it
// encodes, and false if a fence is unclosed or its payload is not base64.
func DecodeFences(s string) (string, bool) {
	var b strings.Builder
	for {
		open := strings.Index(s, SpotlightOpenTag)
		if open < 0 {
			if strings.Contains(s, SpotlightCloseTag) {
				return "", false
			}
			b.WriteString(s)
			return b.String(), true
		}
		if strings.Contains(s[:open], SpotlightCloseTag) {
			return "", false
		}
		b.WriteString(s[:open])
		s = s[open+len(SpotlightOpenTag):]
		end := strings.Index(s, SpotlightCloseTag)
		if end < 0 {
			return "", false
		}
		decoded, err := base64.StdEncoding.DecodeString(s[:end])
		if err != nil {
			return "", false
		}
		b.Write(decoded)
		s = s[end+len(SpotlightCloseTag):]
	}
}

// spotlightFence matches a well-formed fence: the tags around nothing but
// base64. A stray open tag in plain text cannot pair with a later fence's
// close tag, so it cannot hide that fence's payload from ScrubFenced.
var spotlightFence = regexp.MustCompile(regexp.QuoteMeta(SpotlightOpenTag) + `([A-Za-z0-9+/]*={0,2})` + regexp.QuoteMeta(SpotlightCloseTag))

// ScrubFenced is Scrub for text that may hold spotlight fences: the decoded
// payload of each well-formed fence is scrubbed and re-encoded, so a secret
// the harness fenced for the model is redacted wherever the text is
// persisted. Text that is not a well-formed fence is left to the plain pass.
func ScrubFenced(value string) string {
	if !strings.Contains(value, SpotlightOpenTag) {
		return Scrub(value)
	}
	fenced := spotlightFence.ReplaceAllStringFunc(value, func(fence string) string {
		payload := fence[len(SpotlightOpenTag) : len(fence)-len(SpotlightCloseTag)]
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return fence
		}
		scrubbed := ScrubFenced(string(decoded))
		if scrubbed == string(decoded) {
			return fence
		}
		return Spotlight(scrubbed)
	})
	return Scrub(fenced)
}

package security

import (
	"encoding/base64"
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

// ScrubFenced is Scrub for text that may hold spotlight fences: the decoded
// payload of each fence is scrubbed and re-encoded, so a secret the
// harness fenced for the model is redacted wherever the text is persisted.
// A fence whose payload is not valid base64 is left to the plain pass.
func ScrubFenced(value string) string {
	if !strings.Contains(value, SpotlightOpenTag) {
		return Scrub(value)
	}
	var b strings.Builder
	rest := value
	for {
		open := strings.Index(rest, SpotlightOpenTag)
		if open < 0 {
			break
		}
		payloadStart := open + len(SpotlightOpenTag)
		end := strings.Index(rest[payloadStart:], SpotlightCloseTag)
		if end < 0 {
			break
		}
		payload := rest[payloadStart : payloadStart+end]
		b.WriteString(rest[:payloadStart])
		if decoded, err := base64.StdEncoding.DecodeString(payload); err == nil {
			if scrubbed := ScrubFenced(string(decoded)); scrubbed != string(decoded) {
				payload = base64.StdEncoding.EncodeToString([]byte(scrubbed))
			}
		}
		b.WriteString(payload)
		b.WriteString(SpotlightCloseTag)
		rest = rest[payloadStart+end+len(SpotlightCloseTag):]
	}
	b.WriteString(rest)
	return Scrub(b.String())
}

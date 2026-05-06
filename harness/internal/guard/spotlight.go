package guard

import (
	"encoding/base64"
)

// Spotlight wrapper tags. Exported so callers (the loop, in a later
// chunk) can recognise already-spotlighted content and avoid double
// wrapping when forwarding tool output back into the model.
const (
	SpotlightOpenTag  = "<untrusted_content_b64>"
	SpotlightCloseTag = "</untrusted_content_b64>"
)

// ApplySpotlight wraps content for "spotlighting" per Hines et al.
// (arXiv:2403.14720): it base64-encodes the raw bytes of content and
// surrounds them with sentinel tags. Encoding makes prompt-injection
// payloads inside the content syntactically inert from the model's
// perspective — there are no English instructions left to follow.
//
// ApplySpotlight always re-encodes. The loop only calls this once per
// chunk (at the wrap site, never on already-wrapped content), so a
// "skip if already wrapped" prefix/suffix check is unnecessary — and
// it would be unsafe: an attacker who controls a tool output could
// emit content that *begins* with SpotlightOpenTag and *ends* with
// SpotlightCloseTag but is otherwise plain text, defeating the
// encoding. If a future use case needs idempotency, verify
// authenticity by stripping the tags and attempting base64-decode
// rather than trusting the sentinel pattern.
func ApplySpotlight(content string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	return SpotlightOpenTag + encoded + SpotlightCloseTag
}

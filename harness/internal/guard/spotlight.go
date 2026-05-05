package guard

import (
	"encoding/base64"
	"strings"
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
// The function is idempotent: if content is already wrapped (begins
// with SpotlightOpenTag and ends with SpotlightCloseTag), it is
// returned unchanged. This lets the loop call ApplySpotlight on every
// hop without compounding wrappers.
func ApplySpotlight(content string) string {
	if strings.HasPrefix(content, SpotlightOpenTag) && strings.HasSuffix(content, SpotlightCloseTag) {
		return content
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	return SpotlightOpenTag + encoded + SpotlightCloseTag
}

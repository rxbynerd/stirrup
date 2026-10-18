package guard

import "github.com/rxbynerd/stirrup/harness/internal/security"

// Spotlight wrapper tags. Exported so callers can recognise already-
// spotlighted content and avoid double wrapping.
const (
	SpotlightOpenTag  = security.SpotlightOpenTag
	SpotlightCloseTag = security.SpotlightCloseTag
)

// ApplySpotlight wraps content for "spotlighting" per Hines et al.
// (arXiv:2403.14720): it base64-encodes content and surrounds it with
// sentinel tags, making embedded prompt-injection payloads inert.
//
// ApplySpotlight always re-encodes rather than skipping already-tagged
// content: an attacker-controlled input could spoof the sentinel tags
// around plain text to bypass encoding, so idempotency must never be
// inferred from the tags alone.
func ApplySpotlight(content string) string {
	return security.Spotlight(content)
}

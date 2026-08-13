package provider

import (
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// validateGeminiThinkingLevel rejects a configured thinkingLevel the
// resolved model does not accept, before any wire bytes are sent — the
// same fail-closed posture LintGeminiSchema takes for schema keywords.
// A rejection surfaces as a config error naming the accepted levels
// rather than as an opaque HTTP 400 mid-stream.
//
// An empty allow-list means "not probed for this model": the level passes
// through untouched so a newly released model is never blocked by a stale
// guess. The types layer has already confirmed the level is a member of
// the REST enum.
func validateGeminiThinkingLevel(level, model string, q quirks.ProviderQuirks) error {
	allowed := q.BehaviourFlags.Gemini.ThinkingLevels
	if level == "" || len(allowed) == 0 {
		return nil
	}
	for _, a := range allowed {
		if strings.EqualFold(a, level) {
			return nil
		}
	}
	return fmt.Errorf(
		"gemini: thinking level %q is not supported by model %q (supported: %s)",
		level, model, strings.Join(allowed, ", "))
}

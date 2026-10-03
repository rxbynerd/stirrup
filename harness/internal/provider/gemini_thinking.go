package provider

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// validateGeminiThinkingLevel rejects a configured reasoning effort
// (StreamParams.ReasoningEffort, projected onto thinkingLevel by this
// adapter) the resolved model does not accept, before any wire bytes are
// sent — the same fail-closed posture LintGeminiSchema takes for schema
// keywords. A rejection surfaces as a config error naming the accepted
// levels rather than as an opaque HTTP 400 mid-stream.
//
// An empty allow-list means "not probed for this model": a level inside
// the thinkingLevel REST enum passes through untouched so a newly released
// model is never blocked by a stale guess. Levels beyond that enum ("xhigh",
// "max") have no Gemini spelling and are always rejected.
func validateGeminiThinkingLevel(level, model string, q quirks.ProviderQuirks) error {
	if level == "" {
		return nil
	}
	if !slices.Contains(geminiThinkingLevelEnum, strings.ToLower(level)) {
		return fmt.Errorf(
			"gemini: reasoningEffort %q has no thinkingLevel equivalent for model %q (supported: %s)",
			level, model, strings.Join(geminiThinkingLevelEnum, ", "))
	}
	allowed := q.BehaviourFlags.Gemini.ThinkingLevels
	if len(allowed) == 0 {
		return nil
	}
	for _, a := range allowed {
		if strings.EqualFold(a, level) {
			return nil
		}
	}
	return fmt.Errorf(
		"gemini: reasoningEffort %q is not supported by model %q (supported: %s)",
		level, model, strings.Join(allowed, ", "))
}

// geminiThinkingLevelEnum is the generationConfig.thinkingConfig
// .thinkingLevel REST enum, lower-cased.
var geminiThinkingLevelEnum = []string{"minimal", "low", "medium", "high"}

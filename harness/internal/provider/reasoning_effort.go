package provider

import (
	"fmt"
	"slices"
	"strings"
)

// validateReasoningEffort rejects a configured reasoningEffort outside the
// resolved model's allow-list before any wire bytes are sent, so an
// unsupported level surfaces as a config error naming the accepted levels
// rather than as an HTTP 400 mid-run. An empty allow-list means the model
// has no probed effort control; the level is then dropped by
// projectReasoningEffort, keeping one RunConfig portable across models.
func validateReasoningEffort(providerType, level, model string, allowed []string) error {
	if level == "" || len(allowed) == 0 || slices.Contains(allowed, strings.ToLower(level)) {
		return nil
	}
	return fmt.Errorf(
		"%s: reasoningEffort %q is not supported by model %q (supported: %s)",
		providerType, level, model, strings.Join(allowed, ", "))
}

// projectReasoningEffort returns the wire value for level, or "" when the
// field must be omitted because the level is unset or unadvertised.
func projectReasoningEffort(level string, allowed []string) string {
	level = strings.ToLower(level)
	if level == "" || !slices.Contains(allowed, level) {
		return ""
	}
	return level
}

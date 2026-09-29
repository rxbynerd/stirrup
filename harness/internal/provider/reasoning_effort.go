package provider

import (
	"context"
	"fmt"
	"log/slog"
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

// warnDroppedReasoningEffort logs when a configured level is omitted from
// the wire because the model advertises no effort control.
func warnDroppedReasoningEffort(ctx context.Context, logger *slog.Logger, providerType, level, model string, allowed []string) {
	if level == "" || len(allowed) > 0 {
		return
	}
	logger.WarnContext(ctx, "reasoningEffort ignored: model has no known effort control",
		slog.String("provider.type", providerType),
		slog.String("provider.model", model),
		slog.String("reasoning_effort", level),
	)
}

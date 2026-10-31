package provider

import (
	"context"
	"log/slog"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// projectOutputSchema returns the schema an adapter puts on the wire, or
// nil when the request carries none, the resolved model advertises no
// structured-output control, or the schema fails types.OutputSchema
// .Validate. In every nil case the request goes out without a schema and
// the caller's prompt-only JSON contract applies.
func projectOutputSchema(schema *types.OutputSchema, capability quirks.OutputSchemaCapability) *types.OutputSchema {
	if schema == nil || !capability.Supported || schema.Validate() != nil {
		return nil
	}
	return schema
}

// logDroppedOutputSchema records why a requested OutputSchema stays off
// the wire: at debug level when the model has no known control (the
// portable, expected case), at warn level when the schema itself is
// invalid. The schema contents are never logged.
func logDroppedOutputSchema(ctx context.Context, logger *slog.Logger, providerType, model string, schema *types.OutputSchema, capability quirks.OutputSchemaCapability) {
	if schema == nil {
		return
	}
	if !capability.Supported {
		logger.DebugContext(ctx, "outputSchema ignored: model has no known structured output control",
			slog.String("provider.type", providerType),
			slog.String("provider.model", model),
		)
		return
	}
	if err := schema.Validate(); err != nil {
		logger.WarnContext(ctx, "outputSchema ignored: invalid schema",
			slog.String("provider.type", providerType),
			slog.String("provider.model", model),
			slog.String("error", err.Error()),
		)
	}
}

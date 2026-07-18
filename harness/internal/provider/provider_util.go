package provider

import (
	"log/slog"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// AdapterDeps carries the cross-cutting dependencies the factory injects
// into every streaming provider adapter (Anthropic, OpenAICompatible,
// OpenAIResponses, Gemini) after construction. Every field is optional
// and nil-safe at every call site. Registry (*quirks.Registry) is
// deliberately NOT part of this struct: each adapter resolves quirks
// differently (e.g. Gemini's ReplayFields gating), so it stays
// adapter-local.
type AdapterDeps struct {
	Tracer      oteltrace.Tracer
	Metrics     *observability.Metrics
	RetryPolicy RetryPolicy
	Logger      *slog.Logger
}

// ruleDescriptions returns the Description field of each rule in the
// supplied slice, preserving order. Returned as a non-nil empty slice
// when the input is empty so the slog.Any attribute renders as `[]`
// rather than `null` — easier to grep and parse downstream. Shared by
// every adapter's quirks-application log line.
func ruleDescriptions(rules []quirks.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Description)
	}
	return out
}

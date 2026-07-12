package provider

import (
	"log/slog"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// AdapterDeps carries the cross-cutting dependencies the factory injects
// into every streaming provider adapter (Anthropic, OpenAICompatible,
// OpenAIResponses, Gemini) after construction. Each field is optional and
// nil-safe at every call site:
//
//   - Tracer: set by factory.go for span instrumentation; a nil Tracer
//     means no spans are recorded.
//   - Metrics: set by factory.go for metric recording; nil means no
//     recording.
//   - RetryPolicy: set by factory.go (or passed into the constructor, for
//     OpenAICompatibleAdapter) from the resolved provider.retry config;
//     the zero value disables retry (single attempt, no backoff).
//   - Logger: set by factory.go to the run's ScrubHandler-backed logger;
//     nil falls back to slog.Default().
//
// Adapters embed AdapterDeps anonymously so existing field access
// (adapter.Tracer, adapter.Metrics, ...) is unchanged — the struct exists
// to give the four adapters one definition of this dependency set instead
// of four independently-maintained copies. Registry (*quirks.Registry) is
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
// rather than `null` — easier to grep and parse downstream.
//
// Shared by every adapter's quirks-application log line (openai.go,
// openai_responses.go, gemini.go, anthropic.go) — it lived only in
// openai.go historically, which meant three other adapters depended on
// an openai-local helper. Consolidated here so it has exactly one
// definition with no adapter-specific home.
func ruleDescriptions(rules []quirks.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Description)
	}
	return out
}

package context

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// metricRecorder wraps a ContextStrategy and records
// stirrup.context.strategy_runs on every Prepare() call. The strategy
// label ("sliding-window" / "summarise" / "offload-to-file") is
// supplied at construction time because the wrapped strategy itself
// doesn't carry one — the factory is the only place that knows the
// concrete type.
//
// The "kind" label distinguishes runs that produced a compaction event
// from no-op runs (messages already fit within budget). It is read from
// LastCompaction() after Prepare returns.
type metricRecorder struct {
	inner   ContextStrategy
	metrics *observability.Metrics
	strat   string
}

// NewMetricRecorder wraps inner with metric recording. Returns inner
// unchanged when metrics is nil so the wrapper has zero overhead in
// no-metrics deployments. strat is the strategy name forwarded on every
// metric observation.
func NewMetricRecorder(inner ContextStrategy, metrics *observability.Metrics, strat string) ContextStrategy {
	if inner == nil {
		// Match verifier/permission helpers: surface the misuse at
		// the next nil dereference rather than swallowing it here.
		return inner
	}
	if metrics == nil {
		return inner
	}
	return &metricRecorder{
		inner:   inner,
		metrics: metrics,
		strat:   strat,
	}
}

// Prepare delegates to the wrapped strategy and records one
// stirrup.context.strategy_runs observation tagged with the strategy
// name and a kind label ("compaction" if the strategy reported a
// compaction event, "noop" otherwise).
func (r *metricRecorder) Prepare(ctx context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error) {
	out, err := r.inner.Prepare(ctx, messages, budget)

	kind := "noop"
	if r.inner.LastCompaction() != nil {
		kind = "compaction"
	}
	r.metrics.ContextStrategyRuns.Add(ctx, 1, metric.WithAttributes(
		attribute.String("strategy", r.strat),
		attribute.String("kind", kind),
	))

	return out, err
}

// LastCompaction delegates to the wrapped strategy. Without this
// pass-through the loop's compaction-event reporting (which reads
// LastCompaction directly) would always see nil after the wrapper
// intercepted Prepare.
func (r *metricRecorder) LastCompaction() *CompactionEvent {
	return r.inner.LastCompaction()
}

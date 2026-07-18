package verifier

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// metricRecorder wraps a Verifier and records stirrup.verifier.runs and
// stirrup.verifier.duration_ms on every Verify() call. typeStr is supplied
// at construction time because the wrapped verifier itself doesn't carry one.
type metricRecorder struct {
	inner   Verifier
	metrics *observability.Metrics
	typeStr string
}

// NewMetricRecorder wraps inner with metric recording. Returns inner
// unchanged when metrics is nil so the wrapper has zero overhead in
// no-metrics deployments.
func NewMetricRecorder(inner Verifier, metrics *observability.Metrics, typeStr string) Verifier {
	if inner == nil {
		// Return inner (nil) unchanged so misuse surfaces at the next nil
		// dereference rather than being masked here.
		return inner
	}
	if metrics == nil {
		return inner
	}
	return &metricRecorder{
		inner:   inner,
		metrics: metrics,
		typeStr: typeStr,
	}
}

// Verify delegates to the wrapped verifier and records metrics on the
// way out. A non-nil error is treated as passed=false for metric
// purposes; the caller still sees the original error/result pair.
func (r *metricRecorder) Verify(ctx context.Context, vc VerifyContext) (*types.VerificationResult, error) {
	start := time.Now()
	result, err := r.inner.Verify(ctx, vc)
	elapsed := time.Since(start)

	passed := err == nil && result != nil && result.Passed
	r.metrics.VerifierRuns.Add(ctx, 1, metric.WithAttributes(
		attribute.String("type", r.typeStr),
		attribute.Bool("passed", passed),
	))
	r.metrics.VerifierDuration.Record(ctx, float64(elapsed.Milliseconds()), metric.WithAttributes(
		attribute.String("type", r.typeStr),
	))

	return result, err
}

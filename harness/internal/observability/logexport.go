package observability

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// LogExporter owns the OTel logs SDK pipeline that ships structured log
// records to an OTLP/gRPC collector alongside the trace and metric
// exporters. Constructed only when log export is opted in
// (RunConfig.Observability.LogsExport.Type == "otlp"); the stderr path
// remains the default regardless. Close must be called at run end to flush
// the tail of the batch and shut the gRPC connection down.
type LogExporter struct {
	provider *sdklog.LoggerProvider
}

// loggerName is the instrumentation scope name carried on every emitted
// log record. It mirrors the "stirrup-harness" scope used by the trace
// emitter and metrics meter so all three signals share one scope identity.
const loggerName = "stirrup-harness"

// NewLogExporter builds the OTLP/gRPC log pipeline for the given endpoint
// and returns both the LogExporter (so the caller can flush + close it) and
// an slog.Handler that writes each record into that pipeline. The handler
// is a leaf: the caller composes it behind the shared ScrubHandler /
// SpanContextHandler so the OTLP path is scrubbed and trace-correlated
// identically to the stderr path.
//
// headers must already have any "secret://" references resolved (see
// ResolveHeaders) — the SDK sees them unchanged, the same contract the
// trace and metric exporters honour.
//
// resourceOpts threads the run-scoped resource attributes so logs carry the
// same resource identity as the traces and metrics from this run, letting a
// backend join the three signals.
//
// Only OTLP/gRPC is supported; OTLP/HTTP log export is left for a future
// change.
func NewLogExporter(ctx context.Context, endpoint string, headers map[string]string, resourceOpts ResourceOptions) (*LogExporter, slog.Handler, error) {
	exporter, err := buildOTLPLogExporter(ctx, endpoint, headers)
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP log exporter: %w", err)
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(BuildResource(resourceOpts)),
	)

	handler := otelslog.NewHandler(loggerName, otelslog.WithLoggerProvider(provider))

	return &LogExporter{provider: provider}, handler, nil
}

// buildOTLPLogExporter constructs the OTLP/gRPC log exporter, mirroring
// buildOTLPTraceExporter / buildOTLPMetricExporter so all three signals
// dial collectors identically.
func buildOTLPLogExporter(ctx context.Context, endpoint string, headers map[string]string) (*otlploggrpc.Exporter, error) {
	opts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(stripURLScheme(endpoint)),
	}
	if isInsecureEndpoint(endpoint) {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	if len(headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(headers))
	}
	return otlploggrpc.New(ctx, opts...)
}

// Flush forces the batch processor to export any queued records. Used by
// the run-end teardown so the tail of a run's logs is not lost when the
// process exits before the next scheduled batch interval.
func (e *LogExporter) Flush(ctx context.Context) error {
	if e == nil || e.provider == nil {
		return nil
	}
	return e.provider.ForceFlush(ctx)
}

// Close flushes and shuts down the log provider (and the underlying gRPC
// exporter). Safe to call on a nil receiver so the factory can defer it
// unconditionally.
func (e *LogExporter) Close() error {
	if e == nil || e.provider == nil {
		return nil
	}
	return e.provider.Shutdown(context.Background())
}

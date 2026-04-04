package observability

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds all OTel metric instruments for the harness. When metrics are
// disabled (no endpoint configured), all instruments are no-ops via
// noop.MeterProvider. The provider field is nil for no-op instances.
type Metrics struct {
	provider *sdkmetric.MeterProvider // nil for noop

	// Counters
	Runs                 metric.Int64Counter
	Turns                metric.Int64Counter
	TokensInput          metric.Int64Counter
	TokensOutput         metric.Int64Counter
	ToolCalls            metric.Int64Counter
	ToolErrors           metric.Int64Counter
	ProviderRequests     metric.Int64Counter
	ProviderErrors       metric.Int64Counter
	ContextCompactions   metric.Int64Counter
	SecurityEvents       metric.Int64Counter
	VerificationAttempts metric.Int64Counter
	Stalls               metric.Int64Counter

	// Histograms
	RunDuration      metric.Float64Histogram
	TurnDuration     metric.Float64Histogram
	ToolCallDuration metric.Float64Histogram
	ProviderLatency  metric.Float64Histogram
	ProviderTTFB     metric.Float64Histogram

	// UpDownCounter (gauge-like)
	ContextTokens metric.Int64UpDownCounter
}

// NewMetrics creates a Metrics instance backed by an OTLP/gRPC metric exporter
// connected to the given endpoint. The exporter uses insecure connections,
// matching the pattern established by the OTel trace emitter.
func NewMetrics(ctx context.Context, endpoint string) (*Metrics, error) {
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	meter := provider.Meter("stirrup-harness")

	m, err := newMetricsFromMeter(meter, provider)
	if err != nil {
		_ = provider.Shutdown(ctx)
		return nil, err
	}
	return m, nil
}

// NewNoopMetrics returns a Metrics instance where all instruments are no-ops.
// Used when metrics are disabled (no endpoint configured).
func NewNoopMetrics() *Metrics {
	meter := noop.NewMeterProvider().Meter("stirrup-harness")
	// noop meter never returns errors, so we can safely ignore them.
	m, _ := newMetricsFromMeter(meter, nil)
	return m
}

// newMetricsFromMeter registers all instruments on the given meter. When
// provider is nil the instance is treated as no-op (Close is a no-op).
// This constructor is unexported so tests can inject a ManualReader-backed
// MeterProvider for in-memory metric collection.
func newMetricsFromMeter(meter metric.Meter, provider *sdkmetric.MeterProvider) (*Metrics, error) {
	m := &Metrics{provider: provider}
	var err error

	// --- Counters ---

	m.Runs, err = meter.Int64Counter("stirrup.harness.runs",
		metric.WithUnit("{run}"),
		metric.WithDescription("Total agentic loop runs started"),
	)
	if err != nil {
		return nil, err
	}

	m.Turns, err = meter.Int64Counter("stirrup.harness.turns",
		metric.WithUnit("{turn}"),
		metric.WithDescription("Total agentic loop turns executed"),
	)
	if err != nil {
		return nil, err
	}

	m.TokensInput, err = meter.Int64Counter("stirrup.harness.tokens.input",
		metric.WithUnit("{token}"),
		metric.WithDescription("Total input tokens consumed"),
	)
	if err != nil {
		return nil, err
	}

	m.TokensOutput, err = meter.Int64Counter("stirrup.harness.tokens.output",
		metric.WithUnit("{token}"),
		metric.WithDescription("Total output tokens consumed"),
	)
	if err != nil {
		return nil, err
	}

	m.ToolCalls, err = meter.Int64Counter("stirrup.harness.tool_calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("Total tool calls dispatched"),
	)
	if err != nil {
		return nil, err
	}

	m.ToolErrors, err = meter.Int64Counter("stirrup.harness.tool_errors",
		metric.WithUnit("{call}"),
		metric.WithDescription("Total tool calls that failed"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderRequests, err = meter.Int64Counter("stirrup.harness.provider_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Total provider streaming requests"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderErrors, err = meter.Int64Counter("stirrup.harness.provider_errors",
		metric.WithUnit("{request}"),
		metric.WithDescription("Total provider request errors"),
	)
	if err != nil {
		return nil, err
	}

	m.ContextCompactions, err = meter.Int64Counter("stirrup.harness.context_compactions",
		metric.WithUnit("{compaction}"),
		metric.WithDescription("Total context compaction events"),
	)
	if err != nil {
		return nil, err
	}

	m.SecurityEvents, err = meter.Int64Counter("stirrup.harness.security_events",
		metric.WithUnit("{event}"),
		metric.WithDescription("Total security events recorded"),
	)
	if err != nil {
		return nil, err
	}

	m.VerificationAttempts, err = meter.Int64Counter("stirrup.harness.verification_attempts",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("Total verification attempts"),
	)
	if err != nil {
		return nil, err
	}

	m.Stalls, err = meter.Int64Counter("stirrup.harness.stalls",
		metric.WithUnit("{stall}"),
		metric.WithDescription("Total stall-detected loop terminations"),
	)
	if err != nil {
		return nil, err
	}

	// --- Histograms ---

	m.RunDuration, err = meter.Float64Histogram("stirrup.harness.run_duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of complete agentic loop runs"),
	)
	if err != nil {
		return nil, err
	}

	m.TurnDuration, err = meter.Float64Histogram("stirrup.harness.turn_duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of individual turns"),
	)
	if err != nil {
		return nil, err
	}

	m.ToolCallDuration, err = meter.Float64Histogram("stirrup.harness.tool_call_duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of individual tool calls"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderLatency, err = meter.Float64Histogram("stirrup.harness.provider_latency",
		metric.WithUnit("ms"),
		metric.WithDescription("Total provider request latency"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderTTFB, err = meter.Float64Histogram("stirrup.harness.provider_ttfb",
		metric.WithUnit("ms"),
		metric.WithDescription("Provider time to first byte"),
	)
	if err != nil {
		return nil, err
	}

	// --- UpDownCounter ---

	m.ContextTokens, err = meter.Int64UpDownCounter("stirrup.harness.context_tokens",
		metric.WithUnit("{token}"),
		metric.WithDescription("Current context window token usage"),
	)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Close shuts down the underlying MeterProvider, flushing any buffered metrics.
// For no-op instances (provider == nil), this is a no-op.
func (m *Metrics) Close() error {
	if m.provider == nil {
		return nil
	}
	return m.provider.Shutdown(context.Background())
}

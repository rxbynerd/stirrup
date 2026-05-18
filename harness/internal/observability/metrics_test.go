package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsRecording_Counters(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	if err != nil {
		t.Fatalf("newMetricsFromMeter() error: %v", err)
	}

	// Record counter values.
	m.Runs.Add(ctx, 1, metric.WithAttributes(attribute.String("run.mode", "execution")))
	m.Turns.Add(ctx, 3)
	m.TokensInput.Add(ctx, 1500)
	m.TokensOutput.Add(ctx, 200)
	m.ToolCalls.Add(ctx, 5)
	m.ToolErrors.Add(ctx, 1)
	m.ProviderRequests.Add(ctx, 3)
	m.ProviderErrors.Add(ctx, 0)
	m.ProviderRetryOutcomes.Add(ctx, 2, metric.WithAttributes(
		attribute.String("provider.type", "openai"),
		attribute.String("provider.model", "gpt-4o-mini"),
		attribute.String("provider.retry.outcome", "succeeded"),
	))
	m.VerificationAttempts.Add(ctx, 2)
	m.Stalls.Add(ctx, 1)
	m.ContextCompactions.Add(ctx, 2)
	m.SecurityEvents.Add(ctx, 4, metric.WithAttributes(attribute.String("event", "test")))
	// Guard counters — exercised so a regression that silently
	// dropped one of the new instruments would be caught here.
	m.GuardChecks.Add(ctx, 7, metric.WithAttributes(
		attribute.String("guard.phase", "pre_turn"),
		attribute.String("guard.id", "granite-guardian"),
		attribute.String("guard.verdict", "allow"),
	))
	m.GuardErrors.Add(ctx, 2, metric.WithAttributes(
		attribute.String("guard.phase", "pre_tool"),
		attribute.String("guard.id", "granite-guardian"),
	))
	m.GuardSkips.Add(ctx, 4, metric.WithAttributes(
		attribute.String("guard.phase", "pre_turn"),
		attribute.String("guard.id", "granite-guardian"),
		attribute.String("reason", "min_chunk_chars"),
	))
	m.GuardSpotlights.Add(ctx, 3, metric.WithAttributes(
		attribute.String("guard.id", "granite-guardian"),
	))

	// Component-level counters (issue #97) — exercised so a regression
	// that silently dropped one of the new instruments would be caught
	// here. Attributes are representative of what call-site wiring is
	// expected to emit (parent.mode, server.name, strategy, type, etc.).
	m.SubagentSpawns.Add(ctx, 2, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.SubagentTokensInput.Add(ctx, 800, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.SubagentTokensOutput.Add(ctx, 120, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.MCPCalls.Add(ctx, 6, metric.WithAttributes(
		attribute.String("server.name", "test-server"),
		attribute.String("tool.name", "search_docs"),
	))
	m.EditAttempts.Add(ctx, 4, metric.WithAttributes(
		attribute.String("strategy", "search-replace"),
	))
	m.VerifierRuns.Add(ctx, 3, metric.WithAttributes(
		attribute.String("type", "test-runner"),
	))
	m.CodeScannerScans.Add(ctx, 5, metric.WithAttributes(
		attribute.String("scanner", "patterns"),
	))
	m.CodeScannerFindings.Add(ctx, 1, metric.WithAttributes(
		attribute.String("scanner", "patterns"),
		attribute.String("severity", "block"),
	))
	m.PermissionDecisions.Add(ctx, 9, metric.WithAttributes(
		attribute.String("policy", "deny-side-effects"),
		attribute.String("decision", "allow"),
		attribute.String("tool", "read_file"),
	))
	m.ContextStrategyRuns.Add(ctx, 2, metric.WithAttributes(
		attribute.String("strategy", "sliding-window"),
	))

	// Collect metrics.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	// Build a map of metric name -> sum for easier assertion.
	sums := extractInt64Sums(t, rm)

	assertInt64Sum(t, sums, "stirrup.harness.runs", 1)
	assertInt64Sum(t, sums, "stirrup.harness.turns", 3)
	assertInt64Sum(t, sums, "stirrup.harness.tokens.input", 1500)
	assertInt64Sum(t, sums, "stirrup.harness.tokens.output", 200)
	assertInt64Sum(t, sums, "stirrup.harness.tool_calls", 5)
	assertInt64Sum(t, sums, "stirrup.harness.tool_errors", 1)
	assertInt64Sum(t, sums, "stirrup.harness.provider_requests", 3)
	assertInt64Sum(t, sums, "stirrup.harness.provider_errors", 0)
	assertInt64Sum(t, sums, "stirrup.harness.provider_retry_outcomes", 2)
	assertInt64Sum(t, sums, "stirrup.harness.verification_attempts", 2)
	assertInt64Sum(t, sums, "stirrup.harness.stalls", 1)
	assertInt64Sum(t, sums, "stirrup.harness.context_compactions", 2)
	assertInt64Sum(t, sums, "stirrup.harness.security_events", 4)
	assertInt64Sum(t, sums, "stirrup.guard.checks", 7)
	assertInt64Sum(t, sums, "stirrup.guard.errors", 2)
	assertInt64Sum(t, sums, "stirrup.guard.skips", 4)
	assertInt64Sum(t, sums, "stirrup.guard.spotlights", 3)

	// Component-level counters.
	assertInt64Sum(t, sums, "stirrup.subagent.spawns", 2)
	assertInt64Sum(t, sums, "stirrup.subagent.tokens.input", 800)
	assertInt64Sum(t, sums, "stirrup.subagent.tokens.output", 120)
	assertInt64Sum(t, sums, "stirrup.mcp.calls", 6)
	assertInt64Sum(t, sums, "stirrup.edit.attempts", 4)
	assertInt64Sum(t, sums, "stirrup.verifier.runs", 3)
	assertInt64Sum(t, sums, "stirrup.codescanner.scans", 5)
	assertInt64Sum(t, sums, "stirrup.codescanner.findings", 1)
	assertInt64Sum(t, sums, "stirrup.permission.decisions", 9)
	assertInt64Sum(t, sums, "stirrup.context.strategy_runs", 2)
}

func TestMetricsRecording_Histograms(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	if err != nil {
		t.Fatalf("newMetricsFromMeter() error: %v", err)
	}

	// Record histogram values.
	m.RunDuration.Record(ctx, 1500.0, metric.WithAttributes(
		attribute.String("run.mode", "execution"),
		attribute.String("run.outcome", "success"),
	))
	m.TurnDuration.Record(ctx, 250.0)
	m.ToolCallDuration.Record(ctx, 50.0)
	// Guard duration histogram exercised so a regression that dropped
	// the instrument would surface here.
	m.GuardDuration.Record(ctx, 42.0, metric.WithAttributes(
		attribute.String("guard.phase", "pre_turn"),
		attribute.String("guard.id", "granite-guardian"),
	))

	// Component-level histograms (issue #97).
	m.SubagentDuration.Record(ctx, 1200.0, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.MCPDuration.Record(ctx, 75.0, metric.WithAttributes(
		attribute.String("server.name", "test-server"),
		attribute.String("tool.name", "search_docs"),
	))
	m.EditDuration.Record(ctx, 18.0, metric.WithAttributes(
		attribute.String("strategy", "search-replace"),
	))
	m.VerifierDuration.Record(ctx, 350.0, metric.WithAttributes(
		attribute.String("type", "test-runner"),
	))

	// Collect metrics.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	histograms := extractFloat64Histograms(t, rm)

	assertFloat64HistogramCount(t, histograms, "stirrup.harness.run_duration", 1)
	assertFloat64HistogramSum(t, histograms, "stirrup.harness.run_duration", 1500.0)
	assertFloat64HistogramCount(t, histograms, "stirrup.harness.turn_duration", 1)
	assertFloat64HistogramCount(t, histograms, "stirrup.harness.tool_call_duration", 1)
	assertFloat64HistogramCount(t, histograms, "stirrup.guard.duration_ms", 1)
	assertFloat64HistogramSum(t, histograms, "stirrup.guard.duration_ms", 42.0)

	// Component-level histograms.
	assertFloat64HistogramCount(t, histograms, "stirrup.subagent.duration_ms", 1)
	assertFloat64HistogramSum(t, histograms, "stirrup.subagent.duration_ms", 1200.0)
	assertFloat64HistogramCount(t, histograms, "stirrup.mcp.duration_ms", 1)
	assertFloat64HistogramSum(t, histograms, "stirrup.mcp.duration_ms", 75.0)
	assertFloat64HistogramCount(t, histograms, "stirrup.edit.duration_ms", 1)
	assertFloat64HistogramSum(t, histograms, "stirrup.edit.duration_ms", 18.0)
	assertFloat64HistogramCount(t, histograms, "stirrup.verifier.duration_ms", 1)
	assertFloat64HistogramSum(t, histograms, "stirrup.verifier.duration_ms", 350.0)
}

// TestMetricsRecording_Resource is the regression test for the
// "unknown_service:stirrup" bug on the metrics side: when no Resource is
// attached to the MeterProvider, OTel-aware backends label the metric
// stream with the SDK fallback service name. We assert here that the
// resource carried alongside collected metrics carries service.name=stirrup
// so this can't silently regress on the metrics path either.
//
// We also assert the issue #95 attributes (deployment.environment,
// service.namespace) reach the metric resource — without these, any
// Grafana group-by-environment or per-namespace dashboard query produces
// nothing because the metric stream has no such labels. The default-value
// path is exercised here; the explicit-options path is exercised by
// TestMetricsRecording_ResourceWithExplicitOptions.
func TestMetricsRecording_Resource(t *testing.T) {
	// Pin env-var fallbacks to empty so the defaults are deterministic
	// regardless of what the developer's shell happens to set.
	t.Setenv(envEnvironment, "")
	t.Setenv(envServiceNamespace, "")

	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(BuildResource(ResourceOptions{})),
	)
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	if err != nil {
		t.Fatalf("newMetricsFromMeter() error: %v", err)
	}
	m.Runs.Add(ctx, 1)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if rm.Resource == nil {
		t.Fatal("ResourceMetrics.Resource is nil — MeterProvider was constructed without WithResource")
	}
	got := make(map[string]string)
	for _, kv := range rm.Resource.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got["service.name"] != ServiceName {
		t.Errorf("service.name=%q, want %q", got["service.name"], ServiceName)
	}
	if got["service.version"] == "" {
		t.Errorf("service.version missing from metrics resource")
	}
	if got["service.instance.id"] == "" {
		t.Errorf("service.instance.id missing from metrics resource")
	}
	if got["deployment.environment"] != DefaultEnvironment {
		t.Errorf("deployment.environment=%q, want %q", got["deployment.environment"], DefaultEnvironment)
	}
	if got["service.namespace"] != DefaultServiceNamespace {
		t.Errorf("service.namespace=%q, want %q", got["service.namespace"], DefaultServiceNamespace)
	}
}

// TestMetricsRecording_ResourceWithExplicitOptions locks down the issue #95
// acceptance criterion that operator-supplied ResourceOptions reach the
// metric resource end-to-end. If a future refactor stopped threading
// resourceOpts into the MeterProvider's Resource (e.g. dropped the
// WithResource call in NewMetrics), this test would catch it.
func TestMetricsRecording_ResourceWithExplicitOptions(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	resOpts := ResourceOptions{
		Environment:      "staging",
		ServiceNamespace: "stirrup-eval",
		RunMode:          "execution",
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(BuildResource(resOpts)),
	)
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	if err != nil {
		t.Fatalf("newMetricsFromMeter() error: %v", err)
	}
	m.Runs.Add(ctx, 1)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if rm.Resource == nil {
		t.Fatal("ResourceMetrics.Resource is nil")
	}

	got := make(map[string]string)
	for _, kv := range rm.Resource.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got["deployment.environment"] != "staging" {
		t.Errorf("deployment.environment=%q, want staging", got["deployment.environment"])
	}
	if got["service.namespace"] != "stirrup-eval" {
		t.Errorf("service.namespace=%q, want stirrup-eval", got["service.namespace"])
	}
	if got["harness.run.mode"] != "execution" {
		t.Errorf("harness.run.mode=%q, want execution", got["harness.run.mode"])
	}
}

func TestNoopMetrics_NoPanic(t *testing.T) {
	ctx := context.Background()
	m := NewNoopMetrics()

	// All of these should be no-ops and must not panic.
	m.Runs.Add(ctx, 1)
	m.Turns.Add(ctx, 5)
	m.TokensInput.Add(ctx, 1000)
	m.TokensOutput.Add(ctx, 200)
	m.ToolCalls.Add(ctx, 3)
	m.ToolErrors.Add(ctx, 1)
	m.ProviderRequests.Add(ctx, 2)
	m.ProviderErrors.Add(ctx, 1)
	m.ProviderRetryOutcomes.Add(ctx, 1)
	m.ContextCompactions.Add(ctx, 1)
	m.SecurityEvents.Add(ctx, 1)
	m.VerificationAttempts.Add(ctx, 1)
	m.Stalls.Add(ctx, 1)
	m.RunDuration.Record(ctx, 1500.0)
	m.TurnDuration.Record(ctx, 250.0)
	m.ToolCallDuration.Record(ctx, 50.0)
	m.ProviderLatency.Record(ctx, 100.0)
	m.ProviderTTFB.Record(ctx, 30.0)

	// Component-level instruments (issue #97). Exercising every new
	// instrument here means a regression that leaves any of them as a
	// nil field in newMetricsFromMeter would surface as a panic on the
	// noop path, rather than waiting for the first production
	// observation (which can be hours into a deployment).
	m.SubagentSpawns.Add(ctx, 1)
	m.SubagentTokensInput.Add(ctx, 100)
	m.SubagentTokensOutput.Add(ctx, 50)
	m.SubagentDuration.Record(ctx, 250.0)
	m.MCPCalls.Add(ctx, 1)
	m.MCPDuration.Record(ctx, 25.0)
	m.EditAttempts.Add(ctx, 1)
	m.EditDuration.Record(ctx, 10.0)
	m.VerifierRuns.Add(ctx, 1)
	m.VerifierDuration.Record(ctx, 75.0)
	m.CodeScannerScans.Add(ctx, 1)
	m.CodeScannerFindings.Add(ctx, 1)
	m.PermissionDecisions.Add(ctx, 1)
	m.ContextStrategyRuns.Add(ctx, 1)

	// ContextTokens is an observable gauge; registering and unregistering
	// a callback on a no-op meter must not panic.
	unregister, err := m.RegisterContextTokensCallback(func() (int64, []attribute.KeyValue) {
		return 0, nil
	})
	if err != nil {
		t.Fatalf("RegisterContextTokensCallback on noop: %v", err)
	}
	unregister()
}

func TestNoopMetrics_CloseIsNoop(t *testing.T) {
	m := NewNoopMetrics()
	if err := m.Close(); err != nil {
		t.Fatalf("Close() on noop metrics returned error: %v", err)
	}
}

func TestMetrics_CloseShutdownsProvider(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	if err != nil {
		t.Fatalf("newMetricsFromMeter() error: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// After shutdown, collecting should fail.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err == nil {
		t.Fatal("expected error collecting after shutdown, got nil")
	}
}

// --- test helpers ---

func extractInt64Sums(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	sums := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				sums[m.Name] = total
			}
		}
	}
	return sums
}

func extractFloat64Histograms(t *testing.T, rm metricdata.ResourceMetrics) map[string]metricdata.Histogram[float64] {
	t.Helper()
	histograms := make(map[string]metricdata.Histogram[float64])
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				histograms[m.Name] = h
			}
		}
	}
	return histograms
}

func assertInt64Sum(t *testing.T, sums map[string]int64, name string, expected int64) {
	t.Helper()
	got, ok := sums[name]
	if !ok {
		t.Errorf("metric %q not found in collected sums", name)
		return
	}
	if got != expected {
		t.Errorf("metric %q: got %d, want %d", name, got, expected)
	}
}

func assertFloat64HistogramCount(t *testing.T, histograms map[string]metricdata.Histogram[float64], name string, expectedCount uint64) {
	t.Helper()
	h, ok := histograms[name]
	if !ok {
		t.Errorf("histogram %q not found", name)
		return
	}
	var totalCount uint64
	for _, dp := range h.DataPoints {
		totalCount += dp.Count
	}
	if totalCount != expectedCount {
		t.Errorf("histogram %q count: got %d, want %d", name, totalCount, expectedCount)
	}
}

func assertFloat64HistogramSum(t *testing.T, histograms map[string]metricdata.Histogram[float64], name string, expectedSum float64) {
	t.Helper()
	h, ok := histograms[name]
	if !ok {
		t.Errorf("histogram %q not found", name)
		return
	}
	var totalSum float64
	for _, dp := range h.DataPoints {
		totalSum += dp.Sum
	}
	if totalSum != expectedSum {
		t.Errorf("histogram %q sum: got %f, want %f", name, totalSum, expectedSum)
	}
}

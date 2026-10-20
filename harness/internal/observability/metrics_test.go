package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsRecording_Counters(t *testing.T) {
	must := require.New(t)
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	must.NoError(err)

	m.Runs.Add(ctx, 1, metric.WithAttributes(attribute.String("run.mode", "execution")))
	m.Turns.Add(ctx, 3)
	m.TokensInput.Add(ctx, 1500)
	m.TokensOutput.Add(ctx, 200)
	m.TokensCacheRead.Add(ctx, 1200)
	m.TokensCacheWrite.Add(ctx, 250)
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
	m.StallNudges.Add(ctx, 2)
	m.WindDowns.Add(ctx, 1)
	m.ContextCompactions.Add(ctx, 2)
	m.SecurityEvents.Add(ctx, 4, metric.WithAttributes(attribute.String("event", "test")))

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

	m.SubagentSpawns.Add(ctx, 2, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.SubagentTokensInput.Add(ctx, 800, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.SubagentTokensOutput.Add(ctx, 120, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.SubagentTokensCacheRead.Add(ctx, 600, metric.WithAttributes(
		attribute.String("parent.mode", "execution"),
	))
	m.SubagentTokensCacheWrite.Add(ctx, 90, metric.WithAttributes(
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

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(ctx, &rm))

	sums := extractInt64Sums(rm)

	for _, tc := range []struct {
		name string
		want int64
	}{
		{"stirrup.harness.runs", 1},
		{"stirrup.harness.turns", 3},
		{"stirrup.harness.tokens.input", 1500},
		{"stirrup.harness.tokens.output", 200},
		{"stirrup.harness.tokens.cache_read", 1200},
		{"stirrup.harness.tokens.cache_write", 250},
		{"stirrup.harness.tool_calls", 5},
		{"stirrup.harness.tool_errors", 1},
		{"stirrup.harness.provider_requests", 3},
		{"stirrup.harness.provider_errors", 0},
		{"stirrup.harness.provider_retry_outcomes", 2},
		{"stirrup.harness.verification_attempts", 2},
		{"stirrup.harness.stalls", 1},
		{"stirrup.harness.stall_nudges", 2},
		{"stirrup.harness.wind_downs", 1},
		{"stirrup.harness.context_compactions", 2},
		{"stirrup.harness.security_events", 4},
		{"stirrup.guard.checks", 7},
		{"stirrup.guard.errors", 2},
		{"stirrup.guard.skips", 4},
		{"stirrup.guard.spotlights", 3},

		// Component-level counters.
		{"stirrup.subagent.spawns", 2},
		{"stirrup.subagent.tokens.input", 800},
		{"stirrup.subagent.tokens.output", 120},
		{"stirrup.subagent.tokens.cache_read", 600},
		{"stirrup.subagent.tokens.cache_write", 90},
		{"stirrup.mcp.calls", 6},
		{"stirrup.edit.attempts", 4},
		{"stirrup.verifier.runs", 3},
		{"stirrup.codescanner.scans", 5},
		{"stirrup.codescanner.findings", 1},
		{"stirrup.permission.decisions", 9},
		{"stirrup.context.strategy_runs", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, ok := sums[tc.name]
			must.True(ok, "metric not collected")
			must.Equal(tc.want, got)
		})
	}
}

func TestMetricsRecording_Histograms(t *testing.T) {
	must := require.New(t)
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	must.NoError(err)

	m.RunDuration.Record(ctx, 1500.0, metric.WithAttributes(
		attribute.String("run.mode", "execution"),
		attribute.String("run.outcome", "success"),
	))
	m.TurnDuration.Record(ctx, 250.0)
	m.ToolCallDuration.Record(ctx, 50.0)

	m.GuardDuration.Record(ctx, 42.0, metric.WithAttributes(
		attribute.String("guard.phase", "pre_turn"),
		attribute.String("guard.id", "granite-guardian"),
	))

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

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(ctx, &rm))

	histograms := extractFloat64Histograms(rm)

	t.Run("count", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			want uint64
		}{
			{"stirrup.harness.run_duration", 1},
			{"stirrup.harness.turn_duration", 1},
			{"stirrup.harness.tool_call_duration", 1},
			{"stirrup.guard.duration", 1},

			// Component-level histograms.
			{"stirrup.subagent.duration", 1},
			{"stirrup.mcp.duration", 1},
			{"stirrup.edit.duration", 1},
			{"stirrup.verifier.duration", 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				must := require.New(t)
				h, ok := histograms[tc.name]
				must.True(ok, "histogram not collected")
				var total uint64
				for _, dp := range h.DataPoints {
					total += dp.Count
				}
				must.Equal(tc.want, total)
			})
		}
	})

	t.Run("sum", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			want float64
		}{
			{"stirrup.harness.run_duration", 1500.0},
			{"stirrup.guard.duration", 42.0},

			// Component-level histograms.
			{"stirrup.subagent.duration", 1200.0},
			{"stirrup.mcp.duration", 75.0},
			{"stirrup.edit.duration", 18.0},
			{"stirrup.verifier.duration", 350.0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				must := require.New(t)
				h, ok := histograms[tc.name]
				must.True(ok, "histogram not collected")
				var total float64
				for _, dp := range h.DataPoints {
					total += dp.Sum
				}
				must.InDelta(tc.want, total, 1e-9)
			})
		}
	})
}

// TestMetricsRecording_Resource pins that the resource attached to the
// MeterProvider carries service.name plus the deployment.environment.name /
// service.namespace defaults, so metrics don't fall back to
// "unknown_service:stirrup" or lose their Grafana group-by labels. The
// explicit-options path is exercised by
// TestMetricsRecording_Resource_ExplicitOptions.
func TestMetricsRecording_Resource(t *testing.T) {
	must := require.New(t)
	t.Setenv(envServiceName, "")

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
	must.NoError(err)
	m.Runs.Add(ctx, 1)

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(ctx, &rm))

	must.NotNil(rm.Resource, "MeterProvider was constructed without WithResource")
	got := make(map[string]string)
	for _, kv := range rm.Resource.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}

	t.Run("service.name", func(t *testing.T) {
		must := require.New(t)
		must.Equal(DefaultServiceName, got["service.name"])
	})
	t.Run("service.version", func(t *testing.T) {
		must := require.New(t)
		must.NotEmpty(got["service.version"])
	})
	t.Run("service.instance.id", func(t *testing.T) {
		must := require.New(t)
		must.NotEmpty(got["service.instance.id"])
	})
	t.Run("deployment.environment.name", func(t *testing.T) {
		must := require.New(t)
		must.Equal(DefaultEnvironment, got["deployment.environment.name"])
	})
	t.Run("service.namespace", func(t *testing.T) {
		must := require.New(t)
		must.Equal(DefaultServiceNamespace, got["service.namespace"])
	})
}

// TestMetricsRecording_Resource_ExplicitOptions pins that
// operator-supplied ResourceOptions reach the metric resource end-to-end.
func TestMetricsRecording_Resource_ExplicitOptions(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	m.Runs.Add(ctx, 1)

	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(ctx, &rm))
	must.NotNil(rm.Resource)

	got := make(map[string]string)
	for _, kv := range rm.Resource.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}

	t.Run("deployment.environment.name", func(t *testing.T) {
		must := require.New(t)
		must.Equal("staging", got["deployment.environment.name"])
	})
	t.Run("service.namespace", func(t *testing.T) {
		must := require.New(t)
		must.Equal("stirrup-eval", got["service.namespace"])
	})
	t.Run("harness.run.mode", func(t *testing.T) {
		must := require.New(t)
		must.Equal("execution", got["harness.run.mode"])
	})
}

func TestNoopMetrics_NoPanic(t *testing.T) {
	must := require.New(t)
	ctx := context.Background()
	m := NewNoopMetrics()

	// All instruments must be no-ops here and must not panic.
	m.Runs.Add(ctx, 1)
	m.Turns.Add(ctx, 5)
	m.TokensInput.Add(ctx, 1000)
	m.TokensOutput.Add(ctx, 200)
	m.TokensCacheRead.Add(ctx, 100)
	m.TokensCacheWrite.Add(ctx, 50)
	m.ToolCalls.Add(ctx, 3)
	m.ToolErrors.Add(ctx, 1)
	m.ProviderRequests.Add(ctx, 2)
	m.ProviderErrors.Add(ctx, 1)
	m.ProviderRetryOutcomes.Add(ctx, 1)
	m.ContextCompactions.Add(ctx, 1)
	m.SecurityEvents.Add(ctx, 1)
	m.VerificationAttempts.Add(ctx, 1)
	m.Stalls.Add(ctx, 1)
	m.StallNudges.Add(ctx, 1)
	m.WindDowns.Add(ctx, 1)
	m.RunDuration.Record(ctx, 1500.0)
	m.TurnDuration.Record(ctx, 250.0)
	m.ToolCallDuration.Record(ctx, 50.0)
	m.ProviderLatency.Record(ctx, 100.0)
	m.ProviderTTFB.Record(ctx, 30.0)

	m.GuardChecks.Add(ctx, 1)
	m.GuardErrors.Add(ctx, 1)
	m.GuardSkips.Add(ctx, 1)
	m.GuardSpotlights.Add(ctx, 1)
	m.GuardDuration.Record(ctx, 42.0)

	m.SubagentSpawns.Add(ctx, 1)
	m.SubagentTokensInput.Add(ctx, 100)
	m.SubagentTokensOutput.Add(ctx, 50)
	m.SubagentTokensCacheRead.Add(ctx, 30)
	m.SubagentTokensCacheWrite.Add(ctx, 10)
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
	must.NoError(err)
	unregister()
}

func TestNoopMetrics_CloseIsNoop(t *testing.T) {
	must := require.New(t)
	m := NewNoopMetrics()
	must.NoError(m.Close())
}

func TestMetrics_Close_ShutsDownProvider(t *testing.T) {
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("test")

	m, err := newMetricsFromMeter(meter, provider)
	must.NoError(err)

	must.NoError(m.Close())

	// After shutdown, collecting should fail.
	var rm metricdata.ResourceMetrics
	must.Error(reader.Collect(context.Background(), &rm))
}

func extractInt64Sums(rm metricdata.ResourceMetrics) map[string]int64 {
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

func extractFloat64Histograms(rm metricdata.ResourceMetrics) map[string]metricdata.Histogram[float64] {
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

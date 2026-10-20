package core

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/types"
)

// logsSettings resolves cfg's telemetry without a platform and returns
// the log exporter settings.
func logsSettings(t *testing.T, cfg *types.RunConfig, headers map[string]string) otlp.Settings {
	t.Helper()
	must := require.New(t)
	exporters, _, err := resolveTelemetry(context.Background(), cfg, headers)
	must.NoError(err)
	return exporters.Logs
}

func TestResolveTelemetry_LogsSettings(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer t"}
	newConfig := func(logsEndpoint string) *types.RunConfig {
		return &types.RunConfig{
			TraceEmitter: types.TraceEmitterConfig{
				Type:     "otel",
				Protocol: otlp.ProtocolHTTPProtobuf,
				Endpoint: "https://gateway.example/otlp",
			},
			Observability: types.ObservabilityConfig{LogsExport: types.LogsExportConfig{
				Type:     "otlp",
				Endpoint: logsEndpoint,
			}},
		}
	}

	t.Run("falls back to the trace emitter settings", func(t *testing.T) {
		must := require.New(t)
		got := logsSettings(t, newConfig(""), headers)
		must.Equal(otlp.ProtocolHTTPProtobuf, got.Protocol)
		must.Equal("https://gateway.example/otlp", got.Endpoint)
		must.Equal("Bearer t", got.Headers["Authorization"])
	})

	t.Run("prefers the logs export endpoint", func(t *testing.T) {
		must := require.New(t)
		got := logsSettings(t, newConfig("https://logs.example/otlp"), headers)
		must.Equal("https://logs.example/otlp", got.Endpoint)
	})
}

// A run whose traces use OTLP/HTTP ships its logs over OTLP/HTTP to the
// same gateway, not over gRPC.
func TestLogExport_HTTPTraceProtocolPostsLogsToV1Logs(t *testing.T) {
	must := require.New(t)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &types.RunConfig{
		TraceEmitter: types.TraceEmitterConfig{
			Type:     "otel",
			Protocol: otlp.ProtocolHTTPProtobuf,
			Endpoint: srv.URL + "/otlp",
		},
		Observability: types.ObservabilityConfig{LogsExport: types.LogsExportConfig{Type: "otlp"}},
	}
	exporter, handler, err := observability.NewLogExporter(context.Background(), logsSettings(t, cfg, nil), observability.ResourceOptions{})
	must.NoError(err)
	slog.New(handler).Info("hello")
	must.NoError(exporter.Close())

	mu.Lock()
	defer mu.Unlock()
	must.NotEmpty(paths, "no request reached the collector")
	must.Equal("POST /otlp/v1/logs", paths[0], "requests = %v", paths)
}

// metricsSettings resolves te's telemetry without a platform. ok is false
// when metrics are not exported.
func metricsSettings(t *testing.T, te types.TraceEmitterConfig, headers map[string]string) (otlp.Settings, bool) {
	t.Helper()
	must := require.New(t)
	cfg := &types.RunConfig{TraceEmitter: te}
	exporters, _, err := resolveTelemetry(context.Background(), cfg, headers)
	must.NoError(err)
	return exporters.Metrics, metricsEnabled(cfg, exporters)
}

func TestResolveTelemetry_MetricsSettings(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer t"}

	t.Run("jsonl emitter exports no metrics", func(t *testing.T) {
		must := require.New(t)
		_, ok := metricsSettings(t, types.TraceEmitterConfig{Type: "jsonl", Endpoint: "localhost:4317"}, headers)
		must.False(ok)
	})

	t.Run("otel emitter without an endpoint exports no metrics", func(t *testing.T) {
		must := require.New(t)
		_, ok := metricsSettings(t, types.TraceEmitterConfig{Type: "otel"}, headers)
		must.False(ok)
	})

	t.Run("otel emitter with an endpoint uses the metrics settings", func(t *testing.T) {
		must := require.New(t)
		got, ok := metricsSettings(t, types.TraceEmitterConfig{
			Type:               "otel",
			Protocol:           otlp.ProtocolHTTPProtobuf,
			Endpoint:           "https://traces.example/otlp",
			MetricsEndpoint:    "https://metrics.example/otlp",
			MetricsTemporality: otlp.TemporalityDelta,
		}, headers)
		must.True(ok)
		must.Equal(otlp.ProtocolHTTPProtobuf, got.Protocol)
		must.Equal("https://metrics.example/otlp", got.Endpoint)
		must.Equal(otlp.TemporalityDelta, got.Temporality)
		must.Equal("Bearer t", got.Headers["Authorization"])
	})

	t.Run("metrics endpoint falls back to the trace endpoint", func(t *testing.T) {
		must := require.New(t)
		got, _ := metricsSettings(t, types.TraceEmitterConfig{Type: "otel", Endpoint: "localhost:4317"}, nil)
		must.Equal("localhost:4317", got.Endpoint)
	})
}

func TestOTelEmitterOptions_MapsConfigAndDefaults(t *testing.T) {
	must := require.New(t)
	headers := map[string]string{"Authorization": "Bearer t"}
	resourceOpts := observability.ResourceOptions{ServiceName: "agent-a"}
	cfg := types.TraceEmitterConfig{
		Type:                   "otel",
		Protocol:               otlp.ProtocolHTTPProtobuf,
		Endpoint:               "https://gateway.example/otlp",
		CaptureContent:         true,
		CaptureContentMaxBytes: 256,
	}
	exporters, _, err := resolveTelemetry(context.Background(), &types.RunConfig{TraceEmitter: cfg}, headers)
	must.NoError(err)

	got := otelEmitterOptions(cfg, exporters, resourceOpts, true)

	t.Run("passes the configured exporter settings", func(t *testing.T) {
		must := require.New(t)
		must.Equal(otlp.ProtocolHTTPProtobuf, got.Exporter.Protocol)
		must.Equal("https://gateway.example/otlp", got.Exporter.Endpoint)
		must.Equal("Bearer t", got.Exporter.Headers["Authorization"])
	})

	t.Run("passes the capture content settings", func(t *testing.T) {
		must := require.New(t)
		must.True(got.CaptureContent)
		must.Equal(256, got.CaptureContentMaxBytes)
	})

	t.Run("passes the resource and redaction settings", func(t *testing.T) {
		must := require.New(t)
		must.Equal("agent-a", got.Resource.ServiceName)
		must.True(got.RedactionDisabled)
	})

	t.Run("defaults the exporter endpoint to localhost:4317", func(t *testing.T) {
		must := require.New(t)
		defaults := types.TraceEmitterConfig{Type: "otel"}
		defaultExporters, _, err := resolveTelemetry(context.Background(), &types.RunConfig{TraceEmitter: defaults}, nil)
		must.NoError(err)
		gotDefaults := otelEmitterOptions(defaults, defaultExporters, resourceOpts, false)
		must.Equal("localhost:4317", gotDefaults.Exporter.Endpoint)
	})
}

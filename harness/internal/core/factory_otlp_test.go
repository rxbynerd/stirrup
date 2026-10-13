package core

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/types"
)

func TestLogExportSettings(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer t"}
	cfg := &types.RunConfig{
		TraceEmitter: types.TraceEmitterConfig{
			Type:     "otel",
			Protocol: otlp.ProtocolHTTPProtobuf,
			Endpoint: "https://gateway.example/otlp",
		},
		Observability: types.ObservabilityConfig{LogsExport: types.LogsExportConfig{Type: "otlp"}},
	}

	got := logExportSettings(cfg, headers)
	if got.Protocol != otlp.ProtocolHTTPProtobuf {
		t.Errorf("Protocol = %q, want the trace emitter's %q", got.Protocol, otlp.ProtocolHTTPProtobuf)
	}
	if got.Endpoint != "https://gateway.example/otlp" {
		t.Errorf("Endpoint = %q, want the trace endpoint fallback", got.Endpoint)
	}
	if got.Headers["Authorization"] != "Bearer t" {
		t.Errorf("Headers = %v, want the resolved trace headers", got.Headers)
	}

	cfg.Observability.LogsExport.Endpoint = "https://logs.example/otlp"
	if got := logExportSettings(cfg, headers); got.Endpoint != "https://logs.example/otlp" {
		t.Errorf("Endpoint = %q, want logsExport.endpoint to win", got.Endpoint)
	}
}

// A run whose traces use OTLP/HTTP ships its logs over OTLP/HTTP to the
// same gateway, not over gRPC.
func TestLogExport_HTTPTraceProtocolPostsLogsToV1Logs(t *testing.T) {
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
	exporter, handler, err := observability.NewLogExporter(context.Background(), logExportSettings(cfg, nil), observability.ResourceOptions{})
	if err != nil {
		t.Fatalf("NewLogExporter: %v", err)
	}
	slog.New(handler).Info("hello")
	if err := exporter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || paths[0] != "POST /otlp/v1/logs" {
		t.Errorf("requests = %v, want a POST to /otlp/v1/logs", paths)
	}
}

func TestMetricsSettings(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer t"}

	if _, ok := metricsSettings(types.TraceEmitterConfig{Type: "jsonl", Endpoint: "localhost:4317"}, headers); ok {
		t.Error("jsonl emitter: metrics must not be exported")
	}
	if _, ok := metricsSettings(types.TraceEmitterConfig{Type: "otel"}, headers); ok {
		t.Error("otel emitter without an endpoint: metrics must not be exported")
	}

	got, ok := metricsSettings(types.TraceEmitterConfig{
		Type:               "otel",
		Protocol:           otlp.ProtocolHTTPProtobuf,
		Endpoint:           "https://traces.example/otlp",
		MetricsEndpoint:    "https://metrics.example/otlp",
		MetricsTemporality: otlp.TemporalityDelta,
	}, headers)
	if !ok {
		t.Fatal("otel emitter with an endpoint: metrics must be exported")
	}
	want := otlp.Settings{
		Protocol:    otlp.ProtocolHTTPProtobuf,
		Endpoint:    "https://metrics.example/otlp",
		Headers:     headers,
		Temporality: otlp.TemporalityDelta,
	}
	if got.Protocol != want.Protocol || got.Endpoint != want.Endpoint || got.Temporality != want.Temporality || got.Headers["Authorization"] != "Bearer t" {
		t.Errorf("metricsSettings = %+v, want %+v", got, want)
	}

	got, _ = metricsSettings(types.TraceEmitterConfig{Type: "otel", Endpoint: "localhost:4317"}, nil)
	if got.Endpoint != "localhost:4317" {
		t.Errorf("Endpoint = %q, want the trace endpoint fallback", got.Endpoint)
	}
}

func TestOTelEmitterOptions(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer t"}
	resourceOpts := observability.ResourceOptions{ServiceName: "agent-a"}

	got := otelEmitterOptions(types.TraceEmitterConfig{
		Type:                   "otel",
		Protocol:               otlp.ProtocolHTTPProtobuf,
		Endpoint:               "https://gateway.example/otlp",
		CaptureContent:         true,
		CaptureContentMaxBytes: 256,
	}, headers, resourceOpts, true)

	if got.Exporter.Protocol != otlp.ProtocolHTTPProtobuf || got.Exporter.Endpoint != "https://gateway.example/otlp" || got.Exporter.Headers["Authorization"] != "Bearer t" {
		t.Errorf("Exporter = %+v, want the configured protocol, endpoint and headers", got.Exporter)
	}
	if !got.CaptureContent || got.CaptureContentMaxBytes != 256 {
		t.Errorf("CaptureContent = %v, CaptureContentMaxBytes = %d, want true and 256", got.CaptureContent, got.CaptureContentMaxBytes)
	}
	if got.Resource.ServiceName != "agent-a" || !got.RedactionDisabled {
		t.Errorf("Resource = %+v, RedactionDisabled = %v, want the passed values", got.Resource, got.RedactionDisabled)
	}

	if got := otelEmitterOptions(types.TraceEmitterConfig{Type: "otel"}, nil, resourceOpts, false); got.Exporter.Endpoint != "localhost:4317" {
		t.Errorf("default Endpoint = %q, want localhost:4317", got.Exporter.Endpoint)
	}
}

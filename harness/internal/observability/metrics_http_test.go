package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// fakeOTLPMetricsServer is the metrics-side counterpart to the trace
// package's fakeOTLPHTTPServer: captures every POST sent by the
// otlpmetrichttp exporter so tests can assert path + headers.
type fakeOTLPMetricsServer struct {
	mu       sync.Mutex
	requests []capturedMetricsRequest
}

type capturedMetricsRequest struct {
	path        string
	headers     http.Header
	bodyLen     int
	contentType string
}

func (f *fakeOTLPMetricsServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		f.mu.Lock()
		f.requests = append(f.requests, capturedMetricsRequest{
			path:        r.URL.Path,
			headers:     r.Header.Clone(),
			bodyLen:     len(body),
			contentType: r.Header.Get("Content-Type"),
		})
		f.mu.Unlock()

		w.WriteHeader(http.StatusOK)
	})
}

func (f *fakeOTLPMetricsServer) snapshot() []capturedMetricsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedMetricsRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// TestNewMetrics_HTTPProtocol_RoutesToV1Metrics is the metrics-side
// happy-path test for issue #100. Constructing a Metrics with
// protocol="http/protobuf", incrementing a counter, and forcing a
// flush via Close() must POST to /v1/metrics with the configured
// header.
func TestNewMetrics_HTTPProtocol_RoutesToV1Metrics(t *testing.T) {
	fake := &fakeOTLPMetricsServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	const (
		authHeader = "Authorization"
		authValue  = "Basic dGVzdC1tOnRlc3QtbQ=="
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, err := NewMetrics(
		ctx,
		srv.URL,
		"http/protobuf",
		map[string]string{authHeader: authValue},
		ResourceOptions{},
	)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// Increment a counter so the exporter has something non-empty to
	// flush on Shutdown. Without an actual measurement, the periodic
	// reader's first export would be a no-op and the test would race
	// against the first collection cycle.
	m.Runs.Add(ctx, 1, metric.WithAttributes(attribute.String("run.mode", "execution")))
	m.TokensInput.Add(ctx, 42)

	// Close drives a final ForceFlush + Shutdown synchronously, which
	// is what we want here: the test asserts on the captured POSTs
	// immediately after Close returns.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	requests := fake.snapshot()
	if len(requests) == 0 {
		t.Fatalf("expected at least one OTLP/HTTP metrics POST, got 0")
	}
	got := requests[0]
	if !strings.HasSuffix(got.path, "/v1/metrics") {
		t.Errorf("path must end in /v1/metrics, got %q", got.path)
	}
	if got.contentType != "application/x-protobuf" {
		t.Errorf("content-type must be application/x-protobuf, got %q", got.contentType)
	}
	if v := got.headers.Get(authHeader); v != authValue {
		t.Errorf("Authorization header: got %q, want %q", v, authValue)
	}
	if got.bodyLen == 0 {
		t.Errorf("expected non-empty protobuf body, got 0 bytes")
	}
}

// TestNewMetrics_HTTPProtocol_PreservesGatewayPath mirrors the
// trace-side path-preservation test: a Grafana-Cloud–style URL ending
// in /otlp must produce metrics POSTs to /otlp/v1/metrics so the
// gateway's tenant routing is preserved.
func TestNewMetrics_HTTPProtocol_PreservesGatewayPath(t *testing.T) {
	if path := joinMetricsPath("/otlp"); path != "/otlp/v1/metrics" {
		t.Errorf("joinMetricsPath(/otlp) = %q, want /otlp/v1/metrics", path)
	}
	if path := joinMetricsPath("/otlp/"); path != "/otlp/v1/metrics" {
		t.Errorf("joinMetricsPath(/otlp/) = %q (trailing slash should be normalised)", path)
	}
	gatewayURL := "https://otlp-gateway-prod-us-east-0.grafana.net/otlp"
	if got := stripURLScheme(gatewayURL); got != "otlp-gateway-prod-us-east-0.grafana.net" {
		t.Errorf("stripURLScheme(%q) = %q (must be host only)", gatewayURL, got)
	}
	if isInsecureEndpoint(gatewayURL) {
		t.Errorf("isInsecureEndpoint(%q) = true; an https:// URL must use TLS", gatewayURL)
	}
	if !isInsecureEndpoint("http://localhost:4318") {
		t.Errorf("isInsecureEndpoint(http://...) must be true (plaintext)")
	}
	if !isInsecureEndpoint("localhost:4318") {
		t.Errorf("isInsecureEndpoint(no scheme) must be true (plaintext local-collector flow)")
	}
}

// TestNewMetrics_HTTPProtocol_RejectsUnknownProtocol is the parallel
// of the trace-side validation test: an unknown wire-protocol value
// must surface as a clear error rather than silently falling back.
func TestNewMetrics_HTTPProtocol_RejectsUnknownProtocol(t *testing.T) {
	_, err := NewMetrics(
		context.Background(),
		"localhost:4318",
		"json", // not supported
		nil,
		ResourceOptions{},
	)
	if err == nil {
		t.Fatalf("expected error for unsupported protocol, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported OTLP protocol") {
		t.Errorf("error must call out the bad protocol, got %q", err.Error())
	}
}

// TestNewMetrics_GRPCProtocol_AcceptsHeaders is a smoke test that the
// gRPC path also accepts a non-empty headers map without erroring on
// option construction. The actual export round-trip is exercised
// separately because gRPC requires a real gRPC server to capture the
// metadata, which is heavier than this layer wants to set up; the
// no-error result here catches a regression that drops the WithHeaders
// option from the slice.
func TestNewMetrics_GRPCProtocol_AcceptsHeaders(t *testing.T) {
	// Direct exporter construction so we exercise the option-
	// stitching path without paying the OTel SDK's connection
	// timeout on Close. NewMetrics goes through the same path; the
	// extra plumbing is incidental to this test's intent.
	exp, err := buildOTLPMetricExporter(
		context.Background(),
		"127.0.0.1:1",
		"grpc",
		map[string]string{"X-Tenant": "team-a"},
	)
	if err != nil {
		t.Fatalf("buildOTLPMetricExporter(grpc with headers): %v", err)
	}
	if exp == nil {
		t.Fatalf("expected non-nil exporter")
	}
}

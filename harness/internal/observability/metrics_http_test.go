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

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
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

// TestNewMetrics_HTTPProtocol_RoutesToV1Metrics pins that constructing a
// Metrics with protocol="http/protobuf", incrementing a counter, and
// forcing a flush via Close() POSTs to /v1/metrics with the configured
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

	m, err := NewMetrics(ctx, otlp.Settings{
		Protocol: otlp.ProtocolHTTPProtobuf,
		Endpoint: srv.URL,
		Headers:  map[string]string{authHeader: authValue},
	}, ResourceOptions{})
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// Without a measurement, the periodic reader's first export would be
	// a no-op and the test would race against the first collection cycle.
	m.Runs.Add(ctx, 1, metric.WithAttributes(attribute.String("run.mode", "execution")))
	m.TokensInput.Add(ctx, 42)

	// Close drives a final ForceFlush + Shutdown synchronously, so the
	// test can assert on captured POSTs immediately after it returns.
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

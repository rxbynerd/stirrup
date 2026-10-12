package otlp

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
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// collector captures every OTLP/HTTP POST so tests can assert the path,
// headers, and content type the exporters send.
type collector struct {
	mu       sync.Mutex
	requests []capturedRequest
}

type capturedRequest struct {
	path        string
	headers     http.Header
	bodyLen     int
	contentType string
}

func (c *collector) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		c.mu.Lock()
		c.requests = append(c.requests, capturedRequest{
			path:        r.URL.Path,
			headers:     r.Header.Clone(),
			bodyLen:     len(body),
			contentType: r.Header.Get("Content-Type"),
		})
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

func (c *collector) snapshot() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.requests...)
}

// exportOneSpan drives a single span through exp and shuts it down, so
// every request has been sent when it returns.
func exportOneSpan(t *testing.T, exp sdktrace.SpanExporter) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, span := tp.Tracer("otlp-test").Start(context.Background(), "span")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("TracerProvider.Shutdown: %v", err)
	}
}

// exportOneMetric records a counter through exp and shuts the provider
// down, which collects and exports synchronously.
func exportOneMetric(t *testing.T, exp sdkmetric.Exporter) {
	t.Helper()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	counter, err := mp.Meter("otlp-test").Int64Counter("otlp.test.count")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mp.Shutdown(ctx); err != nil {
		t.Fatalf("MeterProvider.Shutdown: %v", err)
	}
}

// exportOneLog emits a log record through exp and shuts the provider
// down, which flushes the batch synchronously.
func exportOneLog(t *testing.T, exp sdklog.Exporter) {
	t.Helper()
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	var rec otellog.Record
	rec.SetBody(attribute.StringValue("hello"))
	lp.Logger("otlp-test").Emit(context.Background(), rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lp.Shutdown(ctx); err != nil {
		t.Fatalf("LoggerProvider.Shutdown: %v", err)
	}
}

func assertOneRequest(t *testing.T, c *collector, wantPath string, wantHeaders map[string]string) {
	t.Helper()
	requests := c.snapshot()
	if len(requests) == 0 {
		t.Fatal("expected at least one OTLP/HTTP POST, got 0")
	}
	got := requests[0]
	if got.path != wantPath {
		t.Errorf("path = %q, want %q", got.path, wantPath)
	}
	if got.contentType != "application/x-protobuf" {
		t.Errorf("content-type = %q, want application/x-protobuf", got.contentType)
	}
	for k, v := range wantHeaders {
		if g := got.headers.Get(k); g != v {
			t.Errorf("header %s = %q, want %q", k, g, v)
		}
	}
	if got.bodyLen == 0 {
		t.Error("expected a non-empty protobuf body")
	}
}

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		raw  string
		want endpoint
	}{
		{"localhost:4317", endpoint{host: "localhost:4317", insecure: true}},
		{"http://collector:4318", endpoint{host: "collector:4318", insecure: true}},
		{"https://gateway.example", endpoint{host: "gateway.example"}},
		{"https://gateway.example/otlp", endpoint{host: "gateway.example", basePath: "/otlp"}},
		{"http://collector:4318/prefix/", endpoint{host: "collector:4318", basePath: "/prefix/", insecure: true}},
		{"collector:4318/prefix", endpoint{host: "collector:4318", basePath: "/prefix", insecure: true}},
	}
	for _, tc := range cases {
		if got := parseEndpoint(tc.raw); got != tc.want {
			t.Errorf("parseEndpoint(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestSignalURLPath(t *testing.T) {
	cases := []struct {
		sig  Signal
		base string
		want string
	}{
		{SignalTraces, "", "/v1/traces"},
		{SignalMetrics, "/otlp", "/otlp/v1/metrics"},
		{SignalLogs, "/otlp/", "/otlp/v1/logs"},
	}
	for _, tc := range cases {
		if got := tc.sig.urlPath(tc.base); got != tc.want {
			t.Errorf("%s.urlPath(%q) = %q, want %q", tc.sig, tc.base, got, tc.want)
		}
	}
}

func TestNewTraceExporter_HTTPRoutesToV1TracesWithHeaders(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	exp, err := NewTraceExporter(context.Background(), Settings{
		Protocol: ProtocolHTTPProtobuf,
		Endpoint: srv.URL,
		Headers:  map[string]string{"Authorization": "Basic dGVzdA=="},
	})
	if err != nil {
		t.Fatalf("NewTraceExporter: %v", err)
	}
	exportOneSpan(t, exp)
	assertOneRequest(t, c, "/v1/traces", map[string]string{"Authorization": "Basic dGVzdA=="})
}

func TestNewMetricExporter_HTTPRoutesToV1MetricsWithHeaders(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	exp, err := NewMetricExporter(context.Background(), Settings{
		Protocol: ProtocolHTTPProtobuf,
		Endpoint: srv.URL,
		Headers:  map[string]string{"Authorization": "Basic dGVzdA=="},
	})
	if err != nil {
		t.Fatalf("NewMetricExporter: %v", err)
	}
	exportOneMetric(t, exp)
	assertOneRequest(t, c, "/v1/metrics", map[string]string{"Authorization": "Basic dGVzdA=="})
}

func TestNewLogExporter_HTTPRoutesToV1LogsWithHeaders(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	exp, err := NewLogExporter(context.Background(), Settings{
		Protocol: ProtocolHTTPProtobuf,
		Endpoint: srv.URL,
		Headers:  map[string]string{"Authorization": "Basic dGVzdA=="},
	})
	if err != nil {
		t.Fatalf("NewLogExporter: %v", err)
	}
	exportOneLog(t, exp)
	assertOneRequest(t, c, "/v1/logs", map[string]string{"Authorization": "Basic dGVzdA=="})
}

// An https:// gateway URL keeps its path prefix and is reached over TLS:
// the TLS test server rejects plaintext, so a downgrade fails the export.
func TestHTTPExporters_HTTPSGatewayKeepsPrefixAndTLS(t *testing.T) {
	c := &collector{}
	srv := httptest.NewTLSServer(c.handler())
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 5 * time.Second
	settings := Settings{
		Protocol:   ProtocolHTTPProtobuf,
		Endpoint:   srv.URL + "/otlp",
		HTTPClient: client,
	}

	traceExp, err := NewTraceExporter(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewTraceExporter: %v", err)
	}
	exportOneSpan(t, traceExp)
	metricExp, err := NewMetricExporter(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewMetricExporter: %v", err)
	}
	exportOneMetric(t, metricExp)
	logExp, err := NewLogExporter(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewLogExporter: %v", err)
	}
	exportOneLog(t, logExp)

	var paths []string
	for _, r := range c.snapshot() {
		paths = append(paths, r.path)
	}
	want := []string{"/otlp/v1/traces", "/otlp/v1/metrics", "/otlp/v1/logs"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("request paths = %v, want %v", paths, want)
	}
}

func TestHTTPExporters_UseSuppliedHTTPClient(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	rt := &countingTransport{base: http.DefaultTransport}
	exp, err := NewTraceExporter(context.Background(), Settings{
		Protocol:   ProtocolHTTPProtobuf,
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Transport: rt, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewTraceExporter: %v", err)
	}
	exportOneSpan(t, exp)
	if rt.count() == 0 {
		t.Fatal("supplied HTTP client was not used for the export")
	}
}

type countingTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	n    int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.base.RoundTrip(r)
}

func (c *countingTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestNewMetricExporter_Temporality(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		temporality string
		env         string
		want        metricdata.Temporality
	}{
		{"default is cumulative", "", "", metricdata.CumulativeTemporality},
		{"env applies when unset", "", "delta", metricdata.DeltaTemporality},
		{"explicit delta", TemporalityDelta, "", metricdata.DeltaTemporality},
		{"explicit field beats env", TemporalityCumulative, "delta", metricdata.CumulativeTemporality},
	}
	for _, tc := range cases {
		for _, protocol := range []string{ProtocolGRPC, ProtocolHTTPProtobuf} {
			t.Run(tc.name+"/"+protocol, func(t *testing.T) {
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", tc.env)
				exp, err := NewMetricExporter(ctx, Settings{
					Protocol:    protocol,
					Endpoint:    "127.0.0.1:1",
					Temporality: tc.temporality,
				})
				if err != nil {
					t.Fatalf("NewMetricExporter: %v", err)
				}
				defer func() { _ = exp.Shutdown(ctx) }()
				if got := exp.Temporality(sdkmetric.InstrumentKindCounter); got != tc.want {
					t.Errorf("counter temporality = %v, want %v", got, tc.want)
				}
			})
		}
	}
}

func TestNewMetricExporter_RejectsUnknownTemporality(t *testing.T) {
	_, err := NewMetricExporter(context.Background(), Settings{Endpoint: "127.0.0.1:1", Temporality: "lowmemory"})
	if err == nil || !strings.Contains(err.Error(), "unsupported metrics temporality") {
		t.Errorf("err = %v, want an unsupported metrics temporality error", err)
	}
}

func TestConstructors_RejectUnknownProtocol(t *testing.T) {
	ctx := context.Background()
	s := Settings{Protocol: "http", Endpoint: "localhost:4318"}
	_, traceErr := NewTraceExporter(ctx, s)
	_, metricErr := NewMetricExporter(ctx, s)
	_, logErr := NewLogExporter(ctx, s)
	for name, err := range map[string]error{"trace": traceErr, "metric": metricErr, "log": logErr} {
		if err == nil || !strings.Contains(err.Error(), "unsupported OTLP protocol") {
			t.Errorf("%s exporter: err = %v, want an unsupported OTLP protocol error", name, err)
		}
	}
}

func TestConstructors_RejectHTTPClientOnGRPC(t *testing.T) {
	ctx := context.Background()
	s := Settings{Protocol: ProtocolGRPC, Endpoint: "localhost:4317", HTTPClient: &http.Client{Timeout: time.Second}}
	_, traceErr := NewTraceExporter(ctx, s)
	_, metricErr := NewMetricExporter(ctx, s)
	_, logErr := NewLogExporter(ctx, s)
	for name, err := range map[string]error{"trace": traceErr, "metric": metricErr, "log": logErr} {
		if err == nil || !strings.Contains(err.Error(), "requires protocol http/protobuf") {
			t.Errorf("%s exporter: err = %v, want an HTTP-client-requires-http/protobuf error", name, err)
		}
	}
}

// gRPC exporters dial lazily, so construction with headers succeeds
// without a collector and proves the header option is accepted.
func TestGRPCConstructors_AcceptHeaders(t *testing.T) {
	ctx := context.Background()
	s := Settings{Endpoint: "127.0.0.1:1", Headers: map[string]string{"X-Tenant": "team-a"}}
	traceExp, err := NewTraceExporter(ctx, s)
	if err != nil {
		t.Fatalf("NewTraceExporter: %v", err)
	}
	metricExp, err := NewMetricExporter(ctx, s)
	if err != nil {
		t.Fatalf("NewMetricExporter: %v", err)
	}
	logExp, err := NewLogExporter(ctx, s)
	if err != nil {
		t.Fatalf("NewLogExporter: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = traceExp.Shutdown(shutdownCtx)
	_ = metricExp.Shutdown(shutdownCtx)
	_ = logExp.Shutdown(shutdownCtx)
}

package otlp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, tp.Shutdown(ctx))
}

// exportOneMetric records a counter through exp and shuts the provider
// down, which collects and exports synchronously.
func exportOneMetric(t *testing.T, exp sdkmetric.Exporter) {
	t.Helper()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	counter, err := mp.Meter("otlp-test").Int64Counter("otlp.test.count")
	require.NoError(t, err)
	counter.Add(context.Background(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mp.Shutdown(ctx))
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
	require.NoError(t, lp.Shutdown(ctx))
}

// assertOneRequest checks the first captured POST. The field checks are
// independent, so each reports on its own.
func assertOneRequest(t *testing.T, c *collector, wantPath string, wantHeaders map[string]string) {
	t.Helper()
	requests := c.snapshot()
	require.NotEmpty(t, requests, "expected at least one OTLP/HTTP POST")
	got := requests[0]
	assert.Equal(t, wantPath, got.path)
	assert.Equal(t, "application/x-protobuf", got.contentType)
	for k, v := range wantHeaders {
		assert.Equal(t, v, got.headers.Get(k), "header %s", k)
	}
	assert.NotZero(t, got.bodyLen, "expected a non-empty protobuf body")
}

func TestParseEndpoint_AcceptsSupportedForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want endpoint
	}{
		{"host and port without scheme", "localhost:4317", endpoint{url: "http://localhost:4317"}},
		{"http with port", "http://collector:4318", endpoint{url: "http://collector:4318"}},
		{"https without path", "https://gateway.example", endpoint{url: "https://gateway.example"}},
		{"https with path prefix", "https://gateway.example/otlp", endpoint{url: "https://gateway.example", basePath: "/otlp"}},
		{"uppercase scheme with path prefix", "HTTPS://gateway.example/otlp", endpoint{url: "https://gateway.example", basePath: "/otlp"}},
		{"path prefix with trailing slash", "http://collector:4318/prefix/", endpoint{url: "http://collector:4318", basePath: "/prefix/"}},
		{"scheme-less host with path prefix", "collector:4318/prefix", endpoint{url: "http://collector:4318", basePath: "/prefix"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEndpoint(tc.raw)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseEndpoint_RejectsWithoutEchoingInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"unsupported ftp scheme", "ftp://collector:4318"},
		{"unsupported grpcs scheme", "grpcs://collector:4317"},
		{"empty host", "https://"},
		{"userinfo in authority", "https://user:SEKRIT@collector"},
		{"query string", "https://collector/otlp?token=SEKRIT"},
		{"fragment", "https://collector/otlp#SEKRIT"},
		{"malformed path escape", "http://collector:4318/%zz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEndpoint(tc.raw)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "SEKRIT", "error echoes the input")
		})
	}
}

func TestConstructors_RejectInvalidEndpoint(t *testing.T) {
	ctx := context.Background()
	for _, protocol := range []string{ProtocolGRPC, ProtocolHTTPProtobuf} {
		t.Run(protocol, func(t *testing.T) {
			s := Settings{Protocol: protocol, Endpoint: "ftp://collector:4318"}
			_, traceErr := NewTraceExporter(ctx, s)
			_, metricErr := NewMetricExporter(ctx, s)
			_, logErr := NewLogExporter(ctx, s)
			for name, err := range map[string]error{"trace": traceErr, "metric": metricErr, "log": logErr} {
				t.Run(name, func(t *testing.T) {
					require.ErrorContains(t, err, "scheme must be http or https")
				})
			}
		})
	}
}

func TestSignal_URLPath(t *testing.T) {
	cases := []struct {
		name string
		sig  Signal
		base string
		want string
	}{
		{"traces without base", SignalTraces, "", "/v1/traces"},
		{"metrics under base path", SignalMetrics, "/otlp", "/otlp/v1/metrics"},
		{"logs under base path with trailing slash", SignalLogs, "/otlp/", "/otlp/v1/logs"},
		{"logs base already ends in signal path", SignalLogs, "/v1/logs", "/v1/logs"},
		{"logs base with signal path and trailing slash", SignalLogs, "/otlp/v1/logs/", "/otlp/v1/logs"},
		{"metrics under a logs path", SignalMetrics, "/v1/logs", "/v1/logs/v1/metrics"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.sig.urlPath(tc.base))
		})
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
	require.NoError(t, err)
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
	require.NoError(t, err)
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
	require.NoError(t, err)
	exportOneLog(t, exp)
	assertOneRequest(t, c, "/v1/logs", map[string]string{"Authorization": "Basic dGVzdA=="})
}

// A per-signal URL, the form OTEL_EXPORTER_OTLP_LOGS_ENDPOINT carries, is
// posted to as given rather than gaining a second signal segment.
func TestNewLogExporter_HTTPPerSignalURLUsedAsGiven(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	exp, err := NewLogExporter(context.Background(), Settings{
		Protocol: ProtocolHTTPProtobuf,
		Endpoint: srv.URL + "/otlp/v1/logs",
	})
	require.NoError(t, err)
	exportOneLog(t, exp)
	assertOneRequest(t, c, "/otlp/v1/logs", nil)
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
	require.NoError(t, err)
	exportOneSpan(t, traceExp)

	metricExp, err := NewMetricExporter(context.Background(), settings)
	require.NoError(t, err)
	exportOneMetric(t, metricExp)

	logExp, err := NewLogExporter(context.Background(), settings)
	require.NoError(t, err)
	exportOneLog(t, logExp)

	var paths []string
	for _, r := range c.snapshot() {
		paths = append(paths, r.path)
	}
	require.Equal(t, []string{"/otlp/v1/traces", "/otlp/v1/metrics", "/otlp/v1/logs"}, paths)
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
	require.NoError(t, err)
	exportOneSpan(t, exp)
	require.NotZero(t, rt.count(), "supplied HTTP client was not used for the export")
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
		t.Run(tc.name, func(t *testing.T) {
			for _, protocol := range []string{ProtocolGRPC, ProtocolHTTPProtobuf} {
				t.Run(protocol, func(t *testing.T) {
					t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", tc.env)
					exp, err := NewMetricExporter(ctx, Settings{
						Protocol:    protocol,
						Endpoint:    "127.0.0.1:1",
						Temporality: tc.temporality,
					})
					require.NoError(t, err)
					defer func() { _ = exp.Shutdown(ctx) }()
					require.Equal(t, tc.want, exp.Temporality(sdkmetric.InstrumentKindCounter))
				})
			}
		})
	}
}

func TestNewMetricExporter_RejectsUnknownTemporality(t *testing.T) {
	_, err := NewMetricExporter(context.Background(), Settings{Endpoint: "127.0.0.1:1", Temporality: "lowmemory"})
	require.ErrorContains(t, err, "unsupported metrics temporality")
}

func TestConstructors_RejectUnknownProtocol(t *testing.T) {
	ctx := context.Background()
	s := Settings{Protocol: "http", Endpoint: "localhost:4318"}
	_, traceErr := NewTraceExporter(ctx, s)
	_, metricErr := NewMetricExporter(ctx, s)
	_, logErr := NewLogExporter(ctx, s)
	for name, err := range map[string]error{"trace": traceErr, "metric": metricErr, "log": logErr} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, err, "unsupported OTLP protocol")
		})
	}
}

func TestConstructors_RejectHTTPClientOnGRPC(t *testing.T) {
	ctx := context.Background()
	s := Settings{Protocol: ProtocolGRPC, Endpoint: "localhost:4317", HTTPClient: &http.Client{Timeout: time.Second}}
	_, traceErr := NewTraceExporter(ctx, s)
	_, metricErr := NewMetricExporter(ctx, s)
	_, logErr := NewLogExporter(ctx, s)
	for name, err := range map[string]error{"trace": traceErr, "metric": metricErr, "log": logErr} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, err, "requires protocol http/protobuf")
		})
	}
}

// gRPC exporters dial lazily, so construction with headers succeeds
// without a collector and proves the header option is accepted.
func TestGRPCConstructors_AcceptHeaders(t *testing.T) {
	ctx := context.Background()
	s := Settings{Endpoint: "127.0.0.1:1", Headers: map[string]string{"X-Tenant": "team-a"}}
	traceExp, err := NewTraceExporter(ctx, s)
	require.NoError(t, err)
	metricExp, err := NewMetricExporter(ctx, s)
	require.NoError(t, err)
	logExp, err := NewLogExporter(ctx, s)
	require.NoError(t, err)
	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = traceExp.Shutdown(shutdownCtx)
	_ = metricExp.Shutdown(shutdownCtx)
	_ = logExp.Shutdown(shutdownCtx)
}

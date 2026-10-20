package trace

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// fakeOTLPHTTPServer captures every POST sent by the OTLP/HTTP
// exporter, recording the path, headers, and (length of) body so tests
// can assert that the trace emitter routed correctly.
type fakeOTLPHTTPServer struct {
	mu       sync.Mutex
	requests []capturedRequest
}

type capturedRequest struct {
	path        string
	headers     http.Header
	bodyLen     int
	contentType string
}

func (f *fakeOTLPHTTPServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		f.mu.Lock()
		f.requests = append(f.requests, capturedRequest{
			path:        r.URL.Path,
			headers:     r.Header.Clone(),
			bodyLen:     len(body),
			contentType: r.Header.Get("Content-Type"),
		})
		f.mu.Unlock()

		// Empty 200 OK is what a real OTLP collector returns on
		// successful protobuf ingest.
		w.WriteHeader(http.StatusOK)
	})
}

func (f *fakeOTLPHTTPServer) snapshot() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// TestNewOTelTraceEmitter_HTTPProtocol_RoutesToV1Traces is the core
// happy-path test: setting protocol="http/protobuf"
// against an HTTP test server and running a Start/RecordTurn/Finish
// cycle must POST to /v1/traces with the configured Authorization
// header. We use httptest.NewServer (plain HTTP) and rely on the
// exporter detecting the http:// scheme and selecting plaintext.
func TestNewOTelTraceEmitter_HTTPProtocol_RoutesToV1Traces(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	fake := &fakeOTLPHTTPServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	const (
		authHeader = "Authorization"
		authValue  = "Basic dGVzdC11c2VyOnRlc3QtcGFzcw=="
	)

	emitter, err := NewOTelTraceEmitter(context.Background(), OTelTraceEmitterOptions{
		Exporter: otlp.Settings{
			Endpoint: srv.URL, // httptest URL is "http://127.0.0.1:NNNN"
			Protocol: otlp.ProtocolHTTPProtobuf,
			Headers:  map[string]string{authHeader: authValue},
		},
	})
	must.NoError(err)
	t.Cleanup(func() { _ = emitter.Close() })

	emitter.Start("run-http-1", &types.RunConfig{
		RunID:    "run-http-1",
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic"},
	})
	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 10, Output: 5},
		StopReason: "end_turn",
		DurationMs: 100,
	})

	// Finish triggers a ForceFlush which drives the batch through the
	// HTTP exporter. Give the SDK a generous deadline — it does
	// retries on transient errors.
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = emitter.Finish(flushCtx, "success")
	must.NoError(err, "Finish")
	// Closing forces a Shutdown (final flush) so we can synchronously
	// inspect the captured POSTs without polling.
	must.NoError(emitter.Close(), "Close")

	requests := fake.snapshot()
	must.NotEmpty(requests, "expected at least one OTLP/HTTP POST")

	// Path must end in /v1/traces — the SDK appends this segment to
	// our base path, mirroring what Grafana Cloud's gateway expects
	// (the configured URL ends in /otlp; the SDK appends the
	// per-signal segment).
	got := requests[0]
	check.True(strings.HasSuffix(got.path, "/v1/traces"), "path must end in /v1/traces, got %q", got.path)
	check.Equal("application/x-protobuf", got.contentType, "content-type must be binary OTLP")
	check.Equal(authValue, got.headers.Get(authHeader), "the OTel SDK must forward configured headers verbatim")
	check.NotZero(got.bodyLen, "expected non-empty protobuf body")
}

// TestNewOTelTraceEmitter_HTTPProtocol_RejectsUnknownProtocol pins the
// validation contract: an unrecognised wire-protocol value at the
// constructor must surface as a clear error rather than silently
// falling back to a default. Operators who typo "http" instead of
// "http/protobuf" should see the typo, not have the request silently
// route to gRPC.
func TestNewOTelTraceEmitter_HTTPProtocol_RejectsUnknownProtocol(t *testing.T) {
	must := require.New(t)
	_, err := NewOTelTraceEmitter(context.Background(), OTelTraceEmitterOptions{
		Exporter: otlp.Settings{
			Endpoint: "localhost:4318",
			Protocol: "http", // not in the closed set
		},
	})
	must.Error(err)
	must.ErrorContains(err, "unsupported OTLP protocol")
}

// TestNewOTelTraceEmitter_HTTPProtocol_HeaderValueDoesNotLeakToSlog is
// a slog-scrubbing assertion. The harness
// resolves secret:// references upstream and passes plaintext bearer
// tokens to the SDK; the SDK must not log header values. This test
// captures slog output during the full Start/RecordTurn/Close cycle
// and asserts that the resolved bearer never appears.
//
// The exporter init path does not currently emit the headers map to
// slog, so this assertion is "no leak" — not "must log".
func TestNewOTelTraceEmitter_HTTPProtocol_HeaderValueDoesNotLeakToSlog(t *testing.T) {
	must := require.New(t)
	fake := &fakeOTLPHTTPServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	// Resolve a secret:// reference through the same helper the
	// factory uses, so the test exercises the production code path
	// (rather than passing a literal bearer to the SDK).
	t.Setenv("STIRRUP_TEST_BEARER", "ultra-secret-bearer-zzy")
	resolved, err := observability.ResolveHeaders(context.Background(),
		security.NewEnvSecretStore(),
		map[string]string{"Authorization": "secret://STIRRUP_TEST_BEARER"})
	must.NoError(err)

	// Route slog through a buffer for the duration of the test.
	var logBuf bytes.Buffer
	originalLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	scrubbed := observability.NewScrubHandler(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(slog.New(scrubbed))

	emitter, err := NewOTelTraceEmitter(context.Background(), OTelTraceEmitterOptions{
		Exporter: otlp.Settings{
			Endpoint: srv.URL,
			Protocol: otlp.ProtocolHTTPProtobuf,
			Headers:  resolved,
		},
	})
	must.NoError(err)
	emitter.Start("run-leak-1", &types.RunConfig{
		RunID:    "run-leak-1",
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic"},
	})
	emitter.RecordTurn(types.TurnTrace{Turn: 1, DurationMs: 5})
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = emitter.Finish(flushCtx, "success")
	must.NoError(err, "Finish")
	_ = emitter.Close()

	must.NotContains(logBuf.String(), "ultra-secret-bearer-zzy", "resolved bearer must not appear in slog output")
}

// recordingExporter is a span-exporter decorator that records the
// attributes of every span as it reaches export, then delegates.
type recordingExporter struct {
	sdktrace.SpanExporter
	mu    sync.Mutex
	names []string
	attrs map[string]string
}

func (r *recordingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	for _, s := range spans {
		r.names = append(r.names, s.Name())
		for _, kv := range s.Attributes() {
			r.attrs[string(kv.Key)] = kv.Value.String()
		}
	}
	r.mu.Unlock()
	return r.SpanExporter.ExportSpans(ctx, spans)
}

// TestNewOTelTraceEmitter_WrapSpanExporterSeesFinalAttributes pins that
// the decorator sits between the batcher and the OTLP exporter: it sees
// attributes set on the root span after Start, and its output still
// reaches the collector.
func TestNewOTelTraceEmitter_WrapSpanExporterSeesFinalAttributes(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	fake := &fakeOTLPHTTPServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	rec := &recordingExporter{attrs: map[string]string{}}
	emitter, err := NewOTelTraceEmitter(context.Background(), OTelTraceEmitterOptions{
		Exporter: otlp.Settings{Endpoint: srv.URL, Protocol: otlp.ProtocolHTTPProtobuf},
		WrapSpanExporter: func(inner sdktrace.SpanExporter) sdktrace.SpanExporter {
			rec.SpanExporter = inner
			return rec
		},
	})
	must.NoError(err)
	emitter.Start("run-wrap-1", &types.RunConfig{RunID: "run-wrap-1", Mode: "execution"})
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = emitter.Finish(flushCtx, "success")
	must.NoError(err, "Finish")
	must.NoError(emitter.Close(), "Close")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	must.NotEmpty(rec.names, "decorator must see spans")
	check.Equal("success", rec.attrs["run.outcome"], "decorator must see the end-time run.outcome")
	check.NotEmpty(fake.snapshot(), "decorated exporter must reach the collector")
}

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

	"github.com/rxbynerd/stirrup/harness/internal/observability"
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
// happy-path test for issue #100: setting protocol="http/protobuf"
// against an HTTP test server and running a Start/RecordTurn/Finish
// cycle must POST to /v1/traces with the configured Authorization
// header. We use httptest.NewServer (plain HTTP) and rely on the
// emitter detecting the http:// scheme and toggling WithInsecure().
func TestNewOTelTraceEmitter_HTTPProtocol_RoutesToV1Traces(t *testing.T) {
	fake := &fakeOTLPHTTPServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	const (
		authHeader = "Authorization"
		authValue  = "Basic dGVzdC11c2VyOnRlc3QtcGFzcw=="
	)

	emitter, err := NewOTelTraceEmitter(
		context.Background(),
		srv.URL, // httptest URL is "http://127.0.0.1:NNNN"
		"http/protobuf",
		map[string]string{authHeader: authValue},
		observability.ResourceOptions{},
		false,
		false,
	)
	if err != nil {
		t.Fatalf("NewOTelTraceEmitter: %v", err)
	}
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
	if _, err := emitter.Finish(flushCtx, "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// Closing forces a Shutdown (final flush) so we can synchronously
	// inspect the captured POSTs without polling.
	if err := emitter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	requests := fake.snapshot()
	if len(requests) == 0 {
		t.Fatalf("expected at least one OTLP/HTTP POST, got 0")
	}

	// Path must end in /v1/traces — the SDK appends this segment to
	// our base path, mirroring what Grafana Cloud's gateway expects
	// (the configured URL ends in /otlp; the SDK appends the
	// per-signal segment).
	got := requests[0]
	if !strings.HasSuffix(got.path, "/v1/traces") {
		t.Errorf("path must end in /v1/traces, got %q", got.path)
	}
	if got.contentType != "application/x-protobuf" {
		t.Errorf("content-type must be application/x-protobuf (binary OTLP), got %q", got.contentType)
	}
	if v := got.headers.Get(authHeader); v != authValue {
		t.Errorf("Authorization header: got %q, want %q (the OTel SDK must forward configured headers verbatim)", v, authValue)
	}
	if got.bodyLen == 0 {
		t.Errorf("expected non-empty protobuf body, got 0 bytes")
	}
}

// TestNewOTelTraceEmitter_HTTPProtocol_PreservesGatewayPath pins the
// Grafana-Cloud–style configuration path. When the operator
// configures endpoint=https://gateway.example/otlp, traces must POST
// to /otlp/v1/traces (not /v1/traces — that would bypass the
// gateway's tenant routing). httptest.NewTLSServer gives us a TLS
// endpoint so we also verify the emitter does NOT downgrade to plain
// HTTP for an https:// URL.
func TestNewOTelTraceEmitter_HTTPProtocol_PreservesGatewayPath(t *testing.T) {
	fake := &fakeOTLPHTTPServer{}
	srv := httptest.NewTLSServer(fake.handler())
	defer srv.Close()

	// Build a gateway-style URL with a base path.
	gatewayURL := srv.URL + "/otlp"

	// Use the test server's TLS client to bypass cert verification for
	// the in-test self-signed cert. We do this by stuffing the server's
	// transport into the SDK via an envvar that otlptracehttp respects.
	t.Setenv("SSL_CERT_FILE", "")
	// The OTel HTTP exporter uses the default Transport. For this
	// test we accept that the request will fail at TLS handshake
	// (self-signed) — but the captured-request handler will still
	// fire because httptest.Server's TLS terminates inside the
	// process and srv.Client() trusts the cert. We override this by
	// using srv.Certificate(). Since otlptracehttp doesn't expose a
	// custom http.Client, we can't inject the trust pool here.
	//
	// Workaround: Test the path-preservation behaviour via the
	// emitter's URL parsing helpers, not via an actual successful
	// round-trip. The successful POST is exercised by the plain-HTTP
	// test above.
	if path := joinTracesPath("/otlp"); path != "/otlp/v1/traces" {
		t.Errorf("joinTracesPath(/otlp) = %q, want /otlp/v1/traces", path)
	}
	if path := joinTracesPath("/otlp/"); path != "/otlp/v1/traces" {
		t.Errorf("joinTracesPath(/otlp/) = %q (trailing slash should be normalised)", path)
	}
	if got := stripURLScheme(gatewayURL); got == "" || strings.Contains(got, "/") {
		t.Errorf("stripURLScheme(%q) = %q (must be host:port only)", gatewayURL, got)
	}
	if isInsecureEndpoint(gatewayURL) {
		t.Errorf("isInsecureEndpoint(%q) = true; an https:// URL must use TLS", gatewayURL)
	}
	// Per SF-5: the no-scheme endpoint shape (`localhost:4318`) is the
	// typical local-collector flow and must be classified as insecure
	// so the exporter applies WithInsecure(). Without this assertion
	// the no-scheme branch in isInsecureEndpoint had count=0 in the
	// trace package; the metrics package already covers the same case
	// in TestNewMetrics_HTTPProtocol_PreservesGatewayPath.
	if !isInsecureEndpoint("localhost:4318") {
		t.Error("scheme-less endpoint should be treated as insecure")
	}
}

// TestNewOTelTraceEmitter_GRPCProtocol_AcceptsHeaders is the trace-side
// smoke test for the gRPC arm of buildOTLPTraceExporter. The metrics
// package has TestNewMetrics_GRPCProtocol_AcceptsHeaders covering the
// `if len(headers) > 0 { append WithHeaders }` conditional; the trace
// package had no counterpart, leaving that branch at count=0 in
// coverage. Per synthesis SF-4.
//
// This is a constructor-level test, not a factory-level test. The
// validator added in MF-2 rejects the gRPC + non-empty headers
// combination at config-load time (see
// TestValidateRunConfig_HeadersOnGRPCProtocolRejected in the types
// package), but the constructor itself is not a validation surface —
// it's called from factory.go *after* validation. So a constructor
// test exercising "gRPC with non-empty headers does not error on
// option construction" still pins the previously-uncovered branch
// without contradicting the validator's contract: the validator
// prevents ever reaching this code path with non-empty headers in
// production, but a future refactor that drops the WithHeaders
// option from the slice would still surface here.
func TestNewOTelTraceEmitter_GRPCProtocol_AcceptsHeaders(t *testing.T) {
	// Direct exporter construction so we exercise the option-
	// stitching path without paying the OTel SDK's connection
	// timeout on emitter Close. NewOTelTraceEmitter goes through
	// the same path.
	exp, err := buildOTLPTraceExporter(
		context.Background(),
		"127.0.0.1:1",
		"grpc",
		map[string]string{"X-Tenant": "team-a"},
	)
	if err != nil {
		t.Fatalf("buildOTLPTraceExporter(grpc with headers): %v", err)
	}
	if exp == nil {
		t.Fatalf("expected non-nil exporter")
	}
}

// TestNewOTelTraceEmitter_HTTPProtocol_RejectsUnknownProtocol pins the
// validation contract: an unrecognised wire-protocol value at the
// constructor must surface as a clear error rather than silently
// falling back to a default. Operators who typo "http" instead of
// "http/protobuf" should see the typo, not have the request silently
// route to gRPC.
func TestNewOTelTraceEmitter_HTTPProtocol_RejectsUnknownProtocol(t *testing.T) {
	_, err := NewOTelTraceEmitter(
		context.Background(),
		"localhost:4318",
		"http", // not in the closed set
		nil,
		observability.ResourceOptions{},
		false,
		false,
	)
	if err == nil {
		t.Fatalf("expected error for unsupported protocol, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported OTLP protocol") {
		t.Errorf("error must call out the bad protocol, got %q", err.Error())
	}
}

// TestNewOTelTraceEmitter_HTTPProtocol_HeaderValueDoesNotLeakToSlog is
// the slog-scrubbing assertion mandated by issue #100. The harness
// resolves secret:// references upstream and passes plaintext bearer
// tokens to the SDK; the SDK must not log header values. This test
// captures slog output during the full Start/RecordTurn/Close cycle
// and asserts that the resolved bearer never appears.
//
// The exporter init path does not currently emit the headers map to
// slog, so this assertion is "no leak" — not "must log". A future
// regression that started logging the headers map would be caught
// here even before security review.
func TestNewOTelTraceEmitter_HTTPProtocol_HeaderValueDoesNotLeakToSlog(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}

	// Route slog through a buffer for the duration of the test.
	var logBuf bytes.Buffer
	originalLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	scrubbed := observability.NewScrubHandler(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(slog.New(scrubbed))

	emitter, err := NewOTelTraceEmitter(
		context.Background(),
		srv.URL,
		"http/protobuf",
		resolved,
		observability.ResourceOptions{},
		false,
		false,
	)
	if err != nil {
		t.Fatalf("NewOTelTraceEmitter: %v", err)
	}
	emitter.Start("run-leak-1", &types.RunConfig{
		RunID:    "run-leak-1",
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic"},
	})
	emitter.RecordTurn(types.TurnTrace{Turn: 1, DurationMs: 5})
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := emitter.Finish(flushCtx, "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	_ = emitter.Close()

	if strings.Contains(logBuf.String(), "ultra-secret-bearer-zzy") {
		t.Fatalf("resolved bearer leaked into slog output: %s", logBuf.String())
	}
}

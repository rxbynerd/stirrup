package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// tracedRequest is one JSON-RPC request as seen by traceRecordingServer.
type tracedRequest struct {
	Method        string
	Authorization string
	SessionID     string
	Traceparent   string
	Tracestate    string
	Baggage       string
	// Params is the raw params object, so a test can tell an absent _meta
	// key from an empty one.
	Params map[string]json.RawMessage
}

// meta decodes params._meta, returning nil when the key is absent.
func (r tracedRequest) meta(t *testing.T) map[string]string {
	t.Helper()
	raw, ok := r.Params["_meta"]
	if !ok {
		return nil
	}
	var m map[string]string
	require.NoError(t, json.Unmarshal(raw, &m), "decode _meta %s", raw)
	return m
}

type tracedRequestLog struct {
	mu       sync.Mutex
	requests []tracedRequest
}

func (l *tracedRequestLog) get() []tracedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]tracedRequest(nil), l.requests...)
}

const (
	traceServerSessionID = "sess-trace"
	traceServerToken     = "tok-trace"
)

// traceRecordingServer serves tools/list and tools/call, recording the
// headers and params of every request. tools/call echoes the "id" argument
// so concurrent callers can be told apart. With inFlight > 0, no tools/call
// is answered until inFlight of them have arrived, so the callers' requests
// are provably concurrent.
func traceRecordingServer(t *testing.T, tools []mcpTool, inFlight int) (*httptest.Server, *tracedRequestLog) {
	t.Helper()
	log := &tracedRequestLog{}
	var calls int
	allArrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64                      `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.mu.Lock()
		log.requests = append(log.requests, tracedRequest{
			Method:        req.Method,
			Authorization: r.Header.Get("Authorization"),
			SessionID:     r.Header.Get("Mcp-Session-Id"),
			Traceparent:   r.Header.Get("traceparent"),
			Tracestate:    r.Header.Get("tracestate"),
			Baggage:       r.Header.Get("baggage"),
			Params:        req.Params,
		})
		log.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", traceServerSessionID)

		switch req.Method {
		case "tools/list":
			writeJSONRPCResult(w, req.ID, toolsListResult{Tools: tools})
		case "tools/call":
			if inFlight > 0 {
				log.mu.Lock()
				calls++
				if calls == inFlight {
					close(allArrived)
				}
				log.mu.Unlock()
				select {
				case <-allArrived:
				case <-time.After(5 * time.Second):
					http.Error(w, "tools/call barrier timed out", http.StatusServiceUnavailable)
					return
				}
			}
			var args struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(req.Params["arguments"], &args)
			writeJSONRPCResult(w, req.ID, toolsCallResult{
				Content: []contentItem{{Type: "text", Text: args.ID}},
			})
		default:
			writeJSONRPCError(w, req.ID, -32601, "method not found")
		}
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func newRecordingTracer(t *testing.T) (oteltrace.Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test"), exporter
}

// decodeTraceparent parses a W3C traceparent (and optional tracestate) into
// a span context, failing the test when the value is malformed.
func decodeTraceparent(t *testing.T, traceparent, tracestate string) oteltrace.SpanContext {
	t.Helper()
	carrier := propagation.MapCarrier{"traceparent": traceparent}
	if tracestate != "" {
		carrier["tracestate"] = tracestate
	}
	sc := oteltrace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
	require.True(t, sc.IsValid(), "traceparent %q does not decode to a valid span context", traceparent)
	return sc
}

func requireSameSpan(t *testing.T, where string, got, want oteltrace.SpanContext) {
	t.Helper()
	require.Equal(t, want.TraceID(), got.TraceID(), "%s trace ID", where)
	require.Equal(t, want.SpanID(), got.SpanID(), "%s span ID", where)
}

func connectTraceServer(t *testing.T, ctx context.Context, srv *httptest.Server) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	config := types.MCPServerConfig{Name: "trace", URI: srv.URL, APIKeyRef: "secret://MCP_TOKEN"}
	secrets := &stubSecretStore{secrets: map[string]string{"secret://MCP_TOKEN": traceServerToken}}
	require.NoError(t, client.Connect(ctx, config, secrets), "Connect")
	return registry
}

var echoTool = []mcpTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}

func TestCallTool_PropagatesToolSpanInMetaAndHeaders(t *testing.T) {
	tracer, exporter := newRecordingTracer(t)
	srv, log := traceRecordingServer(t, echoTool, 0)
	registry := connectTraceServer(t, context.Background(), srv)

	// A remote parent carrying tracestate, so the test pins that the vendor
	// state is propagated alongside traceparent.
	ts, err := oteltrace.ParseTraceState("vendor=opaque")
	require.NoError(t, err)
	remote := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    oteltrace.TraceID{0x0a, 0x01},
		SpanID:     oteltrace.SpanID{0x0b, 0x01},
		TraceFlags: oteltrace.FlagsSampled,
		TraceState: ts,
		Remote:     true,
	})
	parentCtx := oteltrace.ContextWithRemoteSpanContext(context.Background(), remote)
	member, err := baggage.NewMember("user.id", "alice")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	parentCtx = baggage.ContextWithBaggage(parentCtx, bag)

	spanCtx, span := tracer.Start(parentCtx, "execute_tool mcp_trace_echo")
	_, err = registry.Resolve("mcp_trace_echo").StructuredHandler(spanCtx, json.RawMessage(`{"id":"a"}`))
	require.NoError(t, err, "tools/call")
	span.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	want := spans[0].SpanContext

	requests := log.get()
	var call *tracedRequest
	for i := range requests {
		if requests[i].Method == "tools/call" {
			call = &requests[i]
		}
	}
	require.NotNil(t, call, "server saw no tools/call")

	meta := call.meta(t)
	require.NotNil(t, meta, "tools/call params carry no _meta")

	t.Run("meta traceparent names the tool span", func(t *testing.T) {
		requireSameSpan(t, "_meta.traceparent", decodeTraceparent(t, meta["traceparent"], meta["tracestate"]), want)
	})
	t.Run("traceparent header names the tool span", func(t *testing.T) {
		requireSameSpan(t, "traceparent header", decodeTraceparent(t, call.Traceparent, call.Tracestate), want)
	})
	t.Run("auth and session headers accompany trace headers", func(t *testing.T) {
		require.Equal(t, "Bearer "+traceServerToken, call.Authorization)
		require.Equal(t, traceServerSessionID, call.SessionID)
	})
	t.Run("tracestate reaches meta and header", func(t *testing.T) {
		require.Equal(t, "vendor=opaque", meta["tracestate"])
		require.Equal(t, "vendor=opaque", call.Tracestate)
	})
	t.Run("baggage never reaches the server", func(t *testing.T) {
		require.NotContains(t, meta, "baggage")
		require.Empty(t, call.Baggage)
	})
	t.Run("meta carries only trace context", func(t *testing.T) {
		for k := range meta {
			require.Contains(t, []string{"traceparent", "tracestate"}, k, "unexpected _meta key")
		}
	})
	t.Run("arguments are the caller's input unchanged", func(t *testing.T) {
		require.JSONEq(t, `{"id":"a"}`, string(call.Params["arguments"]))
	})
}

func TestCallTool_ConcurrentCallsGetDistinctParents(t *testing.T) {
	tracer, exporter := newRecordingTracer(t)
	ids := []string{"a", "b"}
	srv, log := traceRecordingServer(t, echoTool, len(ids))
	registry := connectTraceServer(t, context.Background(), srv)
	handler := registry.Resolve("mcp_trace_echo").StructuredHandler

	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spanCtx, span := tracer.Start(context.Background(), "execute_tool "+id)
			defer span.End()
			_, errs[i] = handler(spanCtx, json.RawMessage(`{"id":"`+id+`"}`))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "call %s", ids[i])
	}

	wantByID := map[string]oteltrace.SpanContext{}
	for _, s := range exporter.GetSpans() {
		wantByID[s.Name[len("execute_tool "):]] = s.SpanContext
	}

	seen := map[string]bool{}
	for _, r := range log.get() {
		if r.Method != "tools/call" {
			continue
		}
		var args struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(r.Params["arguments"], &args))
		want, ok := wantByID[args.ID]
		require.True(t, ok, "no span recorded for call %q", args.ID)
		seen[args.ID] = true
		requireSameSpan(t, "call "+args.ID+" _meta", decodeTraceparent(t, r.meta(t)["traceparent"], ""), want)
		requireSameSpan(t, "call "+args.ID+" header", decodeTraceparent(t, r.Traceparent, ""), want)
	}
	require.Len(t, seen, len(ids), "server saw calls %v, want %v", seen, ids)
	require.NotEqual(t, wantByID["a"].SpanID(), wantByID["b"].SpanID(), "both calls ran under the same span")
}

func TestCallTool_NoSpanSendsNoTraceContext(t *testing.T) {
	tracer, _ := newRecordingTracer(t)
	srv, log := traceRecordingServer(t, echoTool, 0)
	registry := connectTraceServer(t, context.Background(), srv)
	handler := registry.Resolve("mcp_trace_echo").StructuredHandler

	// A traced call first, so the untraced one also proves headers do not
	// carry over between requests on the same session.
	spanCtx, span := tracer.Start(context.Background(), "execute_tool traced")
	_, err := handler(spanCtx, json.RawMessage(`{"id":"traced"}`))
	require.NoError(t, err)
	span.End()
	_, err = handler(context.Background(), json.RawMessage(`{"id":"untraced"}`))
	require.NoError(t, err)

	requests := log.get()
	require.NotEmpty(t, requests, "server saw no requests")
	last := requests[len(requests)-1]
	require.Equal(t, "tools/call", last.Method, "last request method")
	require.NotContains(t, last.Params, "_meta")
	require.Empty(t, last.Traceparent)
	require.Empty(t, last.Tracestate)
	for _, r := range requests {
		if r.Method == "tools/list" {
			require.Empty(t, r.Traceparent, "tools/list without a span sent traceparent")
		}
	}
}

func TestConnect_ToolsListCarriesRunSpan(t *testing.T) {
	tracer, exporter := newRecordingTracer(t)
	srv, log := traceRecordingServer(t, echoTool, 0)

	runCtx, runSpan := tracer.Start(context.Background(), "invoke_agent")
	connectTraceServer(t, runCtx, srv)
	runSpan.End()

	spans := exporter.GetSpans()
	require.NotEmpty(t, spans, "invoke_agent span was not exported")
	want := spans[0].SpanContext

	requests := log.get()
	require.Len(t, requests, 1, "server saw %+v, want a single tools/list", requests)
	require.Equal(t, "tools/list", requests[0].Method)
	requireSameSpan(t, "tools/list header", decodeTraceparent(t, requests[0].Traceparent, ""), want)
	require.NotContains(t, requests[0].Params, "_meta", "only tools/call carries _meta")
}

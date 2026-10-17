package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode _meta %s: %v", raw, err)
	}
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
	if !sc.IsValid() {
		t.Fatalf("traceparent %q does not decode to a valid span context", traceparent)
	}
	return sc
}

func requireSameSpan(t *testing.T, where string, got, want oteltrace.SpanContext) {
	t.Helper()
	if got.TraceID() != want.TraceID() || got.SpanID() != want.SpanID() {
		t.Errorf("%s parent = %s/%s, want %s/%s", where, got.TraceID(), got.SpanID(), want.TraceID(), want.SpanID())
	}
}

func connectTraceServer(t *testing.T, ctx context.Context, srv *httptest.Server) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry()
	client := NewClient(registry, srv.Client())
	config := types.MCPServerConfig{Name: "trace", URI: srv.URL, APIKeyRef: "secret://MCP_TOKEN"}
	secrets := &stubSecretStore{secrets: map[string]string{"secret://MCP_TOKEN": traceServerToken}}
	if err := client.Connect(ctx, config, secrets); err != nil {
		t.Fatalf("Connect: %v", err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	remote := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    oteltrace.TraceID{0x0a, 0x01},
		SpanID:     oteltrace.SpanID{0x0b, 0x01},
		TraceFlags: oteltrace.FlagsSampled,
		TraceState: ts,
		Remote:     true,
	})
	parentCtx := oteltrace.ContextWithRemoteSpanContext(context.Background(), remote)
	member, err := baggage.NewMember("user.id", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	parentCtx = baggage.ContextWithBaggage(parentCtx, bag)

	spanCtx, span := tracer.Start(parentCtx, "execute_tool mcp_trace_echo")
	if _, err := registry.Resolve("mcp_trace_echo").StructuredHandler(spanCtx, json.RawMessage(`{"id":"a"}`)); err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	want := spans[0].SpanContext

	var call *tracedRequest
	for _, r := range log.get() {
		if r.Method == "tools/call" {
			call = &r
		}
	}
	if call == nil {
		t.Fatal("server saw no tools/call")
	}

	meta := call.meta(t)
	if meta == nil {
		t.Fatal("tools/call params carry no _meta")
	}
	requireSameSpan(t, "_meta.traceparent", decodeTraceparent(t, meta["traceparent"], meta["tracestate"]), want)
	requireSameSpan(t, "traceparent header", decodeTraceparent(t, call.Traceparent, call.Tracestate), want)
	if call.Authorization != "Bearer "+traceServerToken || call.SessionID != traceServerSessionID {
		t.Errorf("traced tools/call Authorization=%q Mcp-Session-Id=%q, want both alongside the trace headers",
			call.Authorization, call.SessionID)
	}
	if meta["tracestate"] != "vendor=opaque" || call.Tracestate != "vendor=opaque" {
		t.Errorf("tracestate _meta=%q header=%q, want %q in both", meta["tracestate"], call.Tracestate, "vendor=opaque")
	}

	if _, ok := meta["baggage"]; ok {
		t.Errorf("_meta carries baggage %q; baggage must never reach an MCP server", meta["baggage"])
	}
	if call.Baggage != "" {
		t.Errorf("baggage header = %q; baggage must never reach an MCP server", call.Baggage)
	}
	for k := range meta {
		if k != "traceparent" && k != "tracestate" {
			t.Errorf("_meta carries unexpected key %q", k)
		}
	}

	var args map[string]string
	if err := json.Unmarshal(call.Params["arguments"], &args); err != nil || len(args) != 1 || args["id"] != "a" {
		t.Errorf("arguments = %s, want the caller's input unchanged", call.Params["arguments"])
	}
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
		if err != nil {
			t.Fatalf("call %s: %v", ids[i], err)
		}
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
		if err := json.Unmarshal(r.Params["arguments"], &args); err != nil {
			t.Fatal(err)
		}
		want, ok := wantByID[args.ID]
		if !ok {
			t.Fatalf("no span recorded for call %q", args.ID)
		}
		seen[args.ID] = true
		requireSameSpan(t, "call "+args.ID+" _meta", decodeTraceparent(t, r.meta(t)["traceparent"], ""), want)
		requireSameSpan(t, "call "+args.ID+" header", decodeTraceparent(t, r.Traceparent, ""), want)
	}
	if len(seen) != len(ids) {
		t.Fatalf("server saw calls %v, want %v", seen, ids)
	}
	if wantByID["a"].SpanID() == wantByID["b"].SpanID() {
		t.Fatal("both calls ran under the same span")
	}
}

func TestCallTool_NoSpanSendsNoTraceContext(t *testing.T) {
	tracer, _ := newRecordingTracer(t)
	srv, log := traceRecordingServer(t, echoTool, 0)
	registry := connectTraceServer(t, context.Background(), srv)
	handler := registry.Resolve("mcp_trace_echo").StructuredHandler

	// A traced call first, so the untraced one also proves headers do not
	// carry over between requests on the same session.
	spanCtx, span := tracer.Start(context.Background(), "execute_tool traced")
	if _, err := handler(spanCtx, json.RawMessage(`{"id":"traced"}`)); err != nil {
		t.Fatal(err)
	}
	span.End()
	if _, err := handler(context.Background(), json.RawMessage(`{"id":"untraced"}`)); err != nil {
		t.Fatal(err)
	}

	requests := log.get()
	last := requests[len(requests)-1]
	if last.Method != "tools/call" {
		t.Fatalf("last request method = %q, want tools/call", last.Method)
	}
	if _, ok := last.Params["_meta"]; ok {
		t.Errorf("untraced tools/call sent _meta %s", last.Params["_meta"])
	}
	if last.Traceparent != "" || last.Tracestate != "" {
		t.Errorf("untraced tools/call sent traceparent=%q tracestate=%q", last.Traceparent, last.Tracestate)
	}
	for _, r := range requests {
		if r.Method == "tools/list" && r.Traceparent != "" {
			t.Errorf("tools/list without a span sent traceparent %q", r.Traceparent)
		}
	}
}

func TestConnect_ToolsListCarriesRunSpan(t *testing.T) {
	tracer, exporter := newRecordingTracer(t)
	srv, log := traceRecordingServer(t, echoTool, 0)

	runCtx, runSpan := tracer.Start(context.Background(), "invoke_agent")
	connectTraceServer(t, runCtx, srv)
	runSpan.End()

	want := exporter.GetSpans()[0].SpanContext
	requests := log.get()
	if len(requests) != 1 || requests[0].Method != "tools/list" {
		t.Fatalf("server saw %+v, want a single tools/list", requests)
	}
	requireSameSpan(t, "tools/list header", decodeTraceparent(t, requests[0].Traceparent, ""), want)
	if _, ok := requests[0].Params["_meta"]; ok {
		t.Errorf("tools/list sent _meta %s; only tools/call carries it", requests[0].Params["_meta"])
	}
}

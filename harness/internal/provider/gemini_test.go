package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"golang.org/x/oauth2"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// stubTokenSource returns a fixed token (or error) for tests, mirroring
// how oauth2.ReuseTokenSource looks to the adapter at runtime.
type stubTokenSource struct {
	token string
	err   error
	calls atomic.Int64
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return &oauth2.Token{AccessToken: s.token, TokenType: "Bearer"}, nil
}

// makeGeminiData wraps a JSON payload in the SSE `data: ` framing that
// Vertex emits.
func makeGeminiData(payload string) string {
	return "data: " + payload + "\n\n"
}

func newGeminiTestAdapter(srvURL string, ts oauth2.TokenSource) *GeminiAdapter {
	a := NewGeminiAdapter(ts, "test-project", "us-central1", nil)
	a.baseURLOverride = srvURL
	return a
}

// TestGeminiAdapter_StreamSingleText checks the simplest happy path: one
// text part followed by a STOP finish reason produces a single text_delta
// followed by a message_complete with stop_reason=end_turn.
func TestGeminiAdapter_StreamSingleText(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify URL path embeds project + location + model.
		if !strings.Contains(r.URL.Path, "/projects/test-project/locations/us-central1/publishers/google/models/gemini-2.5-pro:streamGenerateContent") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("alt"); got != "sse" {
			t.Errorf("alt query param = %q, want sse", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	ts := &stubTokenSource{token: "test-token"}
	adapter := newGeminiTestAdapter(srv.URL, ts)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" || events[0].Text != "Hello" {
		t.Errorf("event[0] = %+v, want text_delta/Hello", events[0])
	}
	if events[1].Type != "message_complete" || events[1].StopReason != "end_turn" {
		t.Errorf("event[1] = %+v, want message_complete/end_turn", events[1])
	}
}

// TestGeminiAdapter_StreamMultipleTextDeltas verifies that consecutive text
// chunks are passed through as separate text_delta events whose
// concatenation reproduces the model's full output.
func TestGeminiAdapter_StreamMultipleTextDeltas(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	var got string
	textCount := 0
	for _, ev := range events {
		if ev.Type == "text_delta" {
			textCount++
			got += ev.Text
		}
	}
	if textCount != 2 {
		t.Errorf("expected 2 text_delta events, got %d", textCount)
	}
	if got != "Hello" {
		t.Errorf("concatenated text = %q, want %q", got, "Hello")
	}
}

// TestGeminiAdapter_StreamedToolCall exercises the partialArgs streaming
// pattern: progressive snapshots of the cumulative argument object
// followed by a finalising chunk carrying the complete args. The adapter
// must collapse these into a single tool_call event with the final
// argument object and a synthesised gemini-* ID.
func TestGeminiAdapter_StreamedToolCall(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{},"willContinue":true}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{"path":"main.g"},"willContinue":true}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"main.go"},"willContinue":false}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	var toolCalls []types.StreamEvent
	var stop *types.StreamEvent
	for i := range events {
		switch events[i].Type {
		case "tool_call":
			toolCalls = append(toolCalls, events[i])
		case "message_complete":
			stop = &events[i]
		case "error":
			t.Fatalf("unexpected error event: %v", events[i].Error)
		}
	}

	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool_call event, got %d: %+v", len(toolCalls), toolCalls)
	}
	tc := toolCalls[0]
	if tc.Name != "read_file" {
		t.Errorf("tool_call.Name = %q, want read_file", tc.Name)
	}
	if !strings.HasPrefix(tc.ID, "gemini-") {
		t.Errorf("tool_call.ID = %q, want prefix gemini-", tc.ID)
	}
	if tc.Input["path"] != "main.go" {
		t.Errorf("tool_call.Input[path] = %v, want main.go", tc.Input["path"])
	}
	if stop == nil {
		t.Fatal("expected message_complete event")
	}
	// finishReason=STOP with a functionCall present must remap to tool_use
	// so the loop dispatches the call.
	if stop.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop.StopReason)
	}
}

// TestGeminiAdapter_MultiToolCallTurn ensures two tool calls in the same
// turn produce two distinct tool_call events with unique IDs.
func TestGeminiAdapter_MultiToolCallTurn(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"list_directory","args":{"path":"."}}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	var calls []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "tool_call" {
			calls = append(calls, ev)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool_call events, got %d", len(calls))
	}
	if calls[0].Name != "read_file" || calls[1].Name != "list_directory" {
		t.Errorf("tool names = %q, %q; want read_file, list_directory", calls[0].Name, calls[1].Name)
	}
	if calls[0].ID == calls[1].ID {
		t.Errorf("tool_call IDs collided: %q == %q", calls[0].ID, calls[1].ID)
	}
}

// TestGeminiAdapter_MaxTokens pins the MAX_TOKENS finish-reason mapping.
func TestGeminiAdapter_MaxTokens(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"truncated"}]},"finishReason":"MAX_TOKENS"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}
	last := events[len(events)-1]
	if last.Type != "message_complete" || last.StopReason != "max_tokens" {
		t.Errorf("last event = %+v, want message_complete/max_tokens", last)
	}
}

// TestGeminiAdapter_SafetyBlocked ensures that a partial text_delta
// preceding a SAFETY finish reason is forwarded before the stop event,
// and the stop reason is the canonical "safety_blocked" string.
func TestGeminiAdapter_SafetyBlocked(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"SAFETY"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (partial text + stop), got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" || events[0].Text != "partial" {
		t.Errorf("event[0] = %+v, want text_delta/partial", events[0])
	}
	if events[1].Type != "message_complete" || events[1].StopReason != "safety_blocked" {
		t.Errorf("event[1] = %+v, want message_complete/safety_blocked", events[1])
	}
}

// TestGeminiAdapter_ContextCancellationMidStream ensures that cancelling
// the context terminates the stream cleanly with an error event.
func TestGeminiAdapter_ContextCancellationMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		// Hold the connection open until the client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	cancel()
	events := collectEvents(t, ch)
	if len(events) == 0 {
		t.Fatal("expected at least one event after cancellation")
	}
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Errorf("last event type = %q, want error", last.Type)
	}
}

// TestGeminiAdapter_HTTPError verifies that non-200 responses surface as
// errors that include the bounded body excerpt and the status code.
func TestGeminiAdapter_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"invalid token"}}`)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err == nil {
		t.Fatal("expected error from 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status 401, got: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("error should include body excerpt, got: %v", err)
	}
}

// TestGeminiAdapter_HTTPErrorBodyTruncated confirms the 4 KiB cap on the
// error body excerpt.
func TestGeminiAdapter_HTTPErrorBodyTruncated(t *testing.T) {
	largeBody := strings.Repeat("x", 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, largeBody)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Error message must not contain the full 8 KiB body.
	if len(err.Error()) > 4400 {
		t.Errorf("error message too large (%d chars); body excerpt should be capped at 4 KiB", len(err.Error()))
	}
}

// TestGeminiAdapter_ToolInputSizeCap ensures a runaway tool-call
// argument blob does not exhaust memory: when a single snapshot
// exceeds maxToolInputSize the adapter emits an error and exits cleanly.
func TestGeminiAdapter_ToolInputSizeCap(t *testing.T) {
	// One chunk with an oversized args blob — embed a string
	// padding inside a valid JSON object so the snapshot exceeds the
	// 10 MB cap. The scanner buffer (16 MiB) is large enough to
	// receive the line; the cap test is on the per-snapshot byte count.
	huge := strings.Repeat("a", maxToolInputSize+10)
	payload := fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"explode","args":{"x":%q},"willContinue":false}}]}}]}`, huge)
	body := makeGeminiData(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)

	foundError := false
	for _, ev := range events {
		if ev.Type == "error" && ev.Error != nil && strings.Contains(ev.Error.Error(), "tool input exceeds") {
			foundError = true
		}
	}
	if !foundError {
		t.Errorf("expected tool-input cap error, got: %+v", events)
	}
}

// TestGeminiAdapter_AuthorizationHeader checks the request carries the
// "Bearer <token>" Authorization header sourced from the TokenSource.
func TestGeminiAdapter_AuthorizationHeader(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()

	ts := &stubTokenSource{token: "ya29.test-access-token"}
	adapter := newGeminiTestAdapter(srv.URL, ts)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch {
	}

	if seenAuth != "Bearer ya29.test-access-token" {
		t.Errorf("Authorization header = %q, want %q", seenAuth, "Bearer ya29.test-access-token")
	}
	if ts.calls.Load() != 1 {
		t.Errorf("token fetched %d times, want 1", ts.calls.Load())
	}
}

// TestGeminiAdapter_TokenSourceErrorPropagated ensures a TokenSource
// failure aborts the call before any HTTP traffic.
func TestGeminiAdapter_TokenSourceErrorPropagated(t *testing.T) {
	hits := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	ts := &stubTokenSource{err: errors.New("ADC: no credentials available")}
	adapter := newGeminiTestAdapter(srv.URL, ts)

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err == nil {
		t.Fatal("expected error from token source, got nil")
	}
	if !strings.Contains(err.Error(), "ADC") {
		t.Errorf("expected error to wrap token source error, got: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("HTTP server hit %d times despite token failure", hits.Load())
	}
}

// TestGeminiAdapter_UsageMetadata verifies that CandidatesTokenCount on
// the terminal chunk surfaces as OutputTokens on the stop event.
func TestGeminiAdapter_UsageMetadata(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":42,"candidatesTokenCount":17,"totalTokenCount":59}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	var stop *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			stop = &events[i]
		}
	}
	if stop == nil {
		t.Fatal("expected message_complete event")
	}
	if stop.OutputTokens != 17 {
		t.Errorf("OutputTokens = %d, want 17", stop.OutputTokens)
	}
}

// TestGeminiAdapter_BuildURL covers the global vs regional host
// derivation plus the project / location / model substitutions.
func TestGeminiAdapter_BuildURL(t *testing.T) {
	cases := []struct {
		name     string
		project  string
		location string
		model    string
		want     string
	}{
		{
			name:     "global",
			project:  "proj-1",
			location: "global",
			model:    "gemini-2.5-pro",
			want:     "https://aiplatform.googleapis.com/v1/projects/proj-1/locations/global/publishers/google/models/gemini-2.5-pro:streamGenerateContent?alt=sse",
		},
		{
			name:     "regional",
			project:  "proj-2",
			location: "us-central1",
			model:    "gemini-2.5-flash",
			want:     "https://us-central1-aiplatform.googleapis.com/v1/projects/proj-2/locations/us-central1/publishers/google/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewGeminiAdapter(&stubTokenSource{}, tc.project, tc.location, nil)
			if got := a.buildURL(tc.model); got != tc.want {
				t.Errorf("buildURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGeminiAdapter_RecordsLatencyAndTTFB pins the metric instrumentation:
// every Stream call records exactly one latency sample, and every stream
// that produces at least one event records exactly one TTFB sample.
func TestGeminiAdapter_RecordsLatencyAndTTFB(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	adapter.Metrics = metrics

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("Stream(): %v", err)
	}
	for range ch {
	}

	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"); got != 1 {
		t.Errorf("provider_latency count = %d, want 1", got)
	}
	if got := providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"); got != 1 {
		t.Errorf("provider_ttfb count = %d, want 1", got)
	}

	h, ok := providerHistogramFinder(t, reader, "stirrup.harness.provider_latency")
	if !ok || len(h.DataPoints) == 0 {
		t.Fatal("expected at least one provider_latency data point")
	}
	attrs := h.DataPoints[0].Attributes
	if v, ok := attrs.Value("provider.type"); !ok || v.AsString() != "gemini" {
		t.Errorf("provider.type attr = %v ok=%v, want gemini", v.AsString(), ok)
	}
	if v, ok := attrs.Value("provider.model"); !ok || v.AsString() != "gemini-2.5-pro" {
		t.Errorf("provider.model attr = %v ok=%v, want gemini-2.5-pro", v.AsString(), ok)
	}
}

// TestGeminiAdapter_HasTimeout pins the HTTP client timeout shape so a
// future refactor cannot accidentally drop the safety bounds.
func TestGeminiAdapter_HasTimeout(t *testing.T) {
	a := NewGeminiAdapter(&stubTokenSource{}, "p", "global", nil)
	if a.httpClient.Timeout == 0 {
		t.Error("HTTP client should have a non-zero timeout")
	}
	tr, ok := a.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("TLSHandshakeTimeout should be non-zero")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("ResponseHeaderTimeout should be non-zero")
	}
}

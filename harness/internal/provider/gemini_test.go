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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	a := NewGeminiAdapter(bearerFromTokenSource(ts), "test-project", "us-central1", nil)
	a.baseURLOverride = srvURL
	return a
}

// TestGeminiAdapter_StreamSingleText checks the simplest happy path: one
// text part followed by a STOP finish reason produces a single text_delta
// followed by a message_complete with stop_reason=end_turn.
func TestGeminiAdapter_StreamSingleText(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	// The handler runs on the server goroutine, so it reports with assert rather than require.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/projects/test-project/locations/us-central1/publishers/google/models/gemini-2.5-pro:streamGenerateContent")
		assert.Equal(t, "sse", r.URL.Query().Get("alt"), "alt query param")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	ts := &stubTokenSource{token: "test-token"}
	adapter := newGeminiTestAdapter(srv.URL, ts)

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro", MaxTokens: 1024})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Len(t, events, 2, "events: %+v", events)
	require.Equal(t, "text_delta", events[0].Type, "event[0] = %+v", events[0])
	require.Equal(t, "Hello", events[0].Text, "event[0] = %+v", events[0])
	require.Equal(t, "message_complete", events[1].Type, "event[1] = %+v", events[1])
	require.Equal(t, "end_turn", events[1].StopReason, "event[1] = %+v", events[1])
}

// TestGeminiAdapter_StreamMultipleTextDeltas verifies that consecutive text
// chunks are passed through as separate text_delta events whose
// concatenation reproduces the model's full output.
func TestGeminiAdapter_StreamMultipleTextDeltas(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := serveSSE(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	var deltas []string
	for _, ev := range collectEvents(t, ch) {
		if ev.Type == "text_delta" {
			deltas = append(deltas, ev.Text)
		}
	}
	require.Len(t, deltas, 2, "text_delta events")
	require.Equal(t, "Hello", strings.Join(deltas, ""), "concatenated text")
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

	srv := serveSSE(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

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
			require.FailNow(t, "unexpected error event", "%v", events[i].Error)
		}
	}

	require.Len(t, toolCalls, 1, "tool_call events: %+v", toolCalls)
	tc := toolCalls[0]
	require.Equal(t, "read_file", tc.Name, "tool_call.Name")
	require.True(t, strings.HasPrefix(tc.ID, "gemini-"), "tool_call.ID = %q, want prefix gemini-", tc.ID)
	require.Equal(t, "main.go", tc.Input["path"], "tool_call.Input[path]")
	require.NotNil(t, stop, "expected message_complete event")
	// STOP with a functionCall present remaps to tool_use.
	require.Equal(t, "tool_use", stop.StopReason, "stop_reason")
}

// TestGeminiAdapter_NonObjectArgsMarkToolCall covers a gateway that sends
// functionCall.args as something other than a JSON object: the call is
// marked rather than ending the stream.
func TestGeminiAdapter_NonObjectArgsMarkToolCall(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":"{\"path\": \"main.go\",}"}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := serveSSE(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	call := requireMarkedToolCall(t, events, "", "read_file", `"{\"path\": \"main.go\",}"`)
	require.True(t, strings.HasPrefix(call.ID, "gemini-"), "tool_call.ID = %q, want prefix gemini-", call.ID)
	for _, ev := range events {
		if ev.Type == "message_complete" {
			require.Equal(t, "tool_use", ev.StopReason, "stop_reason")
		}
	}
}

// TestGeminiAdapter_MultiToolCallTurn ensures two tool calls in the same
// turn produce two distinct tool_call events with unique IDs.
func TestGeminiAdapter_MultiToolCallTurn(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"list_directory","args":{"path":"."}}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := serveSSE(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	var calls []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "tool_call" {
			calls = append(calls, ev)
		}
	}
	require.Len(t, calls, 2, "tool_call events")
	require.Equal(t, "read_file", calls[0].Name, "first tool name")
	require.Equal(t, "list_directory", calls[1].Name, "second tool name")
	require.NotEqual(t, calls[0].ID, calls[1].ID, "tool_call IDs must be distinct")
}

// TestGeminiAdapter_MaxTokensFinishReason pins the MAX_TOKENS finish-reason mapping.
func TestGeminiAdapter_MaxTokensFinishReason(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"truncated"}]},"finishReason":"MAX_TOKENS"}]}`)
	srv := serveSSE(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.GreaterOrEqual(t, len(events), 2, "expected at least 2 events")
	last := events[len(events)-1]
	require.Equal(t, "message_complete", last.Type, "last event = %+v", last)
	require.Equal(t, "max_tokens", last.StopReason, "last event = %+v", last)
}

// TestGeminiAdapter_SafetyBlocked ensures that a partial text_delta
// preceding a SAFETY finish reason is forwarded before the stop event,
// and the stop reason is the canonical "safety_blocked" string.
func TestGeminiAdapter_SafetyBlocked(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"SAFETY"}]}`)

	srv := serveSSE(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Len(t, events, 2, "expected partial text and stop, got %+v", events)
	require.Equal(t, "text_delta", events[0].Type, "event[0] = %+v", events[0])
	require.Equal(t, "partial", events[0].Text, "event[0] = %+v", events[0])
	require.Equal(t, "message_complete", events[1].Type, "event[1] = %+v", events[1])
	require.Equal(t, "safety_blocked", events[1].StopReason, "event[1] = %+v", events[1])
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

		<-r.Context().Done()
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := adapter.Stream(ctx, types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	cancel()
	events := collectEvents(t, ch)
	require.NotEmpty(t, events, "expected at least one event after cancellation")
	require.Equal(t, "error", events[len(events)-1].Type, "last event type")
}

// TestGeminiAdapter_HTTPErrorIncludesStatusAndBody verifies that non-200 responses surface as
// errors that include the bounded body excerpt and the status code.
func TestGeminiAdapter_HTTPErrorIncludesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"invalid token"}}`)
	}))
	t.Cleanup(srv.Close)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.Error(t, err, "expected error from 401 response")
	require.ErrorContains(t, err, "401", "error should mention status 401")
	require.ErrorContains(t, err, "invalid token", "error should include body excerpt")
}

// TestGeminiAdapter_HTTPErrorBodyTruncated confirms the 4 KiB cap on the
// error body excerpt.
func TestGeminiAdapter_HTTPErrorBodyTruncated(t *testing.T) {
	largeBody := strings.Repeat("x", 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, largeBody)
	}))
	t.Cleanup(srv.Close)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.Error(t, err)
	// Error message must not contain the full 8 KiB body.
	require.LessOrEqual(t, len(err.Error()), 4400, "body excerpt should be capped at 4 KiB")
}

// TestGeminiAdapter_ToolInputSizeCap ensures a runaway tool-call
// argument blob does not exhaust memory: when a single snapshot
// exceeds maxToolInputSize the adapter emits an error and exits cleanly.
func TestGeminiAdapter_ToolInputSizeCapEmitsError(t *testing.T) {
	huge := strings.Repeat("a", maxToolInputSize+10)
	payload := fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"explode","args":{"x":%q},"willContinue":false}}]}}]}`, huge)
	srv := serveSSE(t, makeGeminiData(payload))

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)
	events := collectEvents(t, ch)

	foundError := false
	for _, ev := range events {
		if ev.Type == "error" && ev.Error != nil && strings.Contains(ev.Error.Error(), "tool input exceeds") {
			foundError = true
		}
	}
	require.True(t, foundError, "expected tool-input cap error, got: %+v", events)
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
	require.NoError(t, err)
	for range ch {
	}

	require.Equal(t, "Bearer ya29.test-access-token", seenAuth, "Authorization header")
	require.EqualValues(t, 1, ts.calls.Load(), "token fetch count")
}

// TestGeminiAdapter_TokenSourceErrorPropagated ensures a TokenSource
// failure aborts the call before any HTTP traffic.
func TestGeminiAdapter_TokenSourceErrorPropagated(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	ts := &stubTokenSource{err: errors.New("ADC: no credentials available")}
	adapter := newGeminiTestAdapter(srv.URL, ts)

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.Error(t, err, "expected error from token source")
	require.ErrorContains(t, err, "ADC", "error should wrap token source error")
	require.Zero(t, hits.Load(), "HTTP server hit despite token failure")
}

// TestGeminiAdapter_UsageMetadataOutputTokens verifies that CandidatesTokenCount on
// the terminal chunk surfaces as OutputTokens on the stop event.
func TestGeminiAdapter_UsageMetadataOutputTokens(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":42,"candidatesTokenCount":17,"totalTokenCount":59}}`)
	srv := serveSSE(t, body)

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	var stop *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			stop = &events[i]
		}
	}
	require.NotNil(t, stop, "expected message_complete event")
	require.Equal(t, 17, stop.OutputTokens, "OutputTokens")
}

// TestGeminiAdapter_BuildURL_GlobalAndRegionalHosts covers the global vs regional host
// derivation plus the project / location / model substitutions.
func TestGeminiAdapter_BuildURL_GlobalAndRegionalHosts(t *testing.T) {
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
			a := NewGeminiAdapter(bearerFromTokenSource(&stubTokenSource{}), tc.project, tc.location, nil)
			require.Equal(t, tc.want, a.buildURL(tc.model))
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
	require.NoError(t, err, "NewMetricsForTesting")

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	adapter.Metrics = metrics

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err, "Stream()")
	for range ch {
	}

	require.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "provider_latency count")
	require.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count")
	assertFirstChunkSeries(t, reader, adapter, "gcp.vertex_ai", "gemini-2.5-pro")

	h, ok := providerHistogramFinder(t, reader, "stirrup.harness.provider_latency")
	require.True(t, ok, "expected provider_latency histogram")
	require.NotEmpty(t, h.DataPoints, "expected at least one provider_latency data point")
	attrs := h.DataPoints[0].Attributes
	providerType, ok := attrs.Value("provider.type")
	require.True(t, ok, "provider.type attribute missing")
	require.Equal(t, "gemini", providerType.AsString(), "provider.type attr")
	providerModel, ok := attrs.Value("provider.model")
	require.True(t, ok, "provider.model attribute missing")
	require.Equal(t, "gemini-2.5-pro", providerModel.AsString(), "provider.model attr")
}

// TestGeminiAdapter_DataPrefixVariants pins the SSE `data:` framing parser:
// the canonical `"data: {...}"` form, the no-space `"data:{...}"` form, and
// the pathological case where the JSON value itself starts with "data:"
// must all parse to the same JSON payload.
func TestGeminiAdapter_DataPrefixVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		// wantText is checked only when set. A naive double-TrimPrefix would
		// corrupt a text value that starts with "data:".
		wantText string
	}{
		{
			name: "with space after data prefix",
			body: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]}}]}\n\n" +
				"data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n",
		},
		{
			name: "no space after data prefix",
			body: "data:{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]}}]}\n\n" +
				"data:{\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n",
		},
		{
			name: "value starts with data prefix",
			body: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"data:image/png;base64,abcd\"}]}}]}\n\n" +
				"data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n",
			wantText: "data:image/png;base64,abcd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveSSE(t, tc.body)

			adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
			ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
			require.NoError(t, err)
			events := collectEvents(t, ch)
			var got string
			var stop *types.StreamEvent
			for i := range events {
				switch events[i].Type {
				case "text_delta":
					got += events[i].Text
				case "message_complete":
					stop = &events[i]
				case "error":
					require.FailNow(t, "unexpected error event", "%v", events[i].Error)
				}
			}
			require.NotEmpty(t, got, "no text_delta event received; payload was likely dropped or malformed")
			require.NotNil(t, stop, "expected message_complete")
			if tc.wantText != "" {
				require.Equal(t, tc.wantText, got, "text_delta value should keep the data: prefix")
			}
		})
	}
}

// TestGeminiAdapter_SimultaneousInterleavedToolCalls asserts two concurrent
// tool calls streamed across willContinue=true chunks accumulate into
// separate slots and produce two distinct tool_call events.
func TestGeminiAdapter_SimultaneousInterleavedToolCalls(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"tool_a","partialArgs":{},"willContinue":true}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"tool_b","partialArgs":{},"willContinue":true}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"tool_a","args":{"path":"a.go"},"willContinue":false}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"tool_b","args":{"path":"b.go"},"willContinue":false}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	calls := map[string]types.StreamEvent{}
	for _, ev := range events {
		require.NotEqual(t, "error", ev.Type, "unexpected error event: %v", ev.Error)
		if ev.Type == "tool_call" {
			calls[ev.Name] = ev
		}
	}
	require.Len(t, calls, 2, "tool_call names: %v", calls)
	require.Equal(t, "a.go", calls["tool_a"].Input["path"])
	require.Equal(t, "b.go", calls["tool_b"].Input["path"])
	require.NotEqual(t, calls["tool_a"].ID, calls["tool_b"].ID, "interleaved tool calls must get distinct IDs")
}

// TestGeminiAdapter_PromptFeedbackBlock verifies B4: a chunk with
// promptFeedback.blockReason set and no candidates must surface as a
// message_complete with StopReason=safety_blocked rather than an empty
// stream.
func TestGeminiAdapter_PromptFeedbackBlock(t *testing.T) {
	body := makeGeminiData(`{"promptFeedback":{"blockReason":"SAFETY"}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Len(t, events, 1, "events: %+v", events)
	require.Equal(t, "message_complete", events[0].Type)
	require.Equal(t, "safety_blocked", events[0].StopReason)
}

// TestGeminiAdapter_BuildURLEscapesModel asserts a model name containing a
// slash percent-encodes into the URL rather than rewriting the path shape,
// a defensive check independent of upstream validation.
func TestGeminiAdapter_BuildURLEscapesModel(t *testing.T) {
	a := NewGeminiAdapter(bearerFromTokenSource(&stubTokenSource{}), "test-project", "global", nil)
	got := a.buildURL("model/with/slashes")
	require.Contains(t, got, "%2F", "model slashes must be percent-encoded")
	require.NotContains(t, got, "model/with/slashes", "raw slashes must not reach the URL")
}

// TestGeminiAdapter_MalformedSSEChunk pins the parser-error path: a
// 200 response with an unparseable chunk body must surface as an
// error event and close the channel.
func TestGeminiAdapter_MalformedSSEChunk(t *testing.T) {
	body := "data: {not valid json\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.NotEmpty(t, events, "expected at least one error event")
	require.Equal(t, "error", events[len(events)-1].Type)
}

// TestGeminiAdapter_BlankFunctionCallName pins the empty-name guard: a
// chunk whose functionCall part lacks a name must surface as a single
// error event and abort the stream, rather than silently coalescing into
// a "" slot.
func TestGeminiAdapter_BlankFunctionCallName(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"args":{"path":"x"}}}]}}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	var errEvents, otherEvents []types.StreamEvent
	for _, ev := range events {
		switch ev.Type {
		case "error":
			errEvents = append(errEvents, ev)
		case "tool_call", "message_complete":
			otherEvents = append(otherEvents, ev)
		}
	}
	require.Empty(t, otherEvents, "expected no tool_call or message_complete events")
	require.Len(t, errEvents, 1, "events: %+v", events)
	require.ErrorContains(t, errEvents[0].Error, "functionCall part missing name")
}

// TestGeminiAdapter_ToolCallBufferDrainOnFinishReason verifies the
// drain path: a tool call with willContinue=true followed immediately
// by a finishReason=STOP chunk (without a closing willContinue=false)
// must still surface exactly one tool_call event.
func TestGeminiAdapter_ToolCallBufferDrainOnFinishReason(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{"path":"x.go"},"willContinue":true}}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	var calls []types.StreamEvent
	for _, ev := range events {
		if ev.Type == "tool_call" {
			calls = append(calls, ev)
		}
	}
	require.Len(t, calls, 1, "drain path must emit exactly one tool_call: %+v", calls)
	require.Equal(t, "read_file", calls[0].Name)
	require.Equal(t, "x.go", calls[0].Input["path"])
}

// TestGeminiAdapter_UsageMetadataDerivedFromTotal verifies the fallback
// path in the OutputTokens calculation: when CandidatesTokenCount is
// zero but TotalTokenCount and PromptTokenCount allow derivation,
// the adapter computes total - prompt and reports that.
func TestGeminiAdapter_UsageMetadataDerivedFromTotal(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":0,"totalTokenCount":80}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	var stop *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			stop = &events[i]
		}
	}
	require.NotNil(t, stop, "expected message_complete")
	require.Equal(t, 50, stop.OutputTokens, "total 80 minus prompt 30")
	require.Equal(t, 30, stop.InputTokens, "promptTokenCount")
	require.Equal(t, 0, stop.ReasoningTokens, "no thoughtsTokenCount")
}

// TestGeminiAdapter_RecitationFinishReason pins the RECITATION enum
// mapping. Recitation blocks are a distinct outcome from safety blocks
// (Vertex emits them when the model would otherwise reproduce
// copyrighted training data) and the trace must distinguish them.
func TestGeminiAdapter_RecitationFinishReason(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"finishReason":"RECITATION"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Len(t, events, 1, "events: %+v", events)
	require.Equal(t, "message_complete", events[0].Type)
	require.Equal(t, "recitation_blocked", events[0].StopReason)
}

// TestGeminiAdapter_EmptyStream verifies the adapter handles a 200 OK
// with an empty body cleanly: the channel closes with no events and
// no error. Mirrors the OpenAI adapter's behaviour.
func TestGeminiAdapter_EmptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	require.Empty(t, events, "empty body must yield no events")
}

// TestGeminiAdapter_StreamingClientTimeouts pins the streaming client shape:
// no total deadline, with bounded TLS handshake and response-header phases.
func TestGeminiAdapter_StreamingClientTimeouts(t *testing.T) {
	a := NewGeminiAdapter(bearerFromTokenSource(&stubTokenSource{}), "p", "global", nil)
	require.Zero(t, a.httpClient.Timeout, "no total deadline: idleTimeoutBody bounds long streams")
	tr, ok := a.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport")
	require.NotZero(t, tr.TLSHandshakeTimeout, "TLS handshake timeout")
	require.NotZero(t, tr.ResponseHeaderTimeout, "response header timeout")
}

// TestGeminiAdapter_NonStreamedFunctionCall3xShape pins the parser against
// the wire format Gemini 3.x emits when streamFunctionCallArguments is
// false: one SSE chunk carrying a functionCall part with both `name` and
// `args` populated, alongside finishReason="STOP" on the same candidate.
// Derived from a real captured Vertex AI response against
// gemini-3.1-pro-preview.
func TestGeminiAdapter_NonStreamedFunctionCall3xShape(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"docs/safety-rings.md"}}}]},"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	var toolCalls []types.StreamEvent
	var stop *types.StreamEvent
	for i := range events {
		require.NotEqual(t, "error", events[i].Type, "unexpected error event: %v", events[i].Error)
		switch events[i].Type {
		case "tool_call":
			toolCalls = append(toolCalls, events[i])
		case "message_complete":
			stop = &events[i]
		}
	}

	require.Len(t, toolCalls, 1, "tool_call events: %+v", toolCalls)
	tc := toolCalls[0]
	require.Equal(t, "read_file", tc.Name)
	require.Equal(t, "docs/safety-rings.md", tc.Input["path"])

	require.NotNil(t, stop, "expected message_complete event")
	require.Equal(t, "tool_use", stop.StopReason)
}

// TestGeminiAdapter_ThoughtSignatureCapturedOnToolCall asserts a
// `thoughtSignature` blob on a functionCall part surfaces on the emitted
// tool_call StreamEvent for next-turn round-trip.
func TestGeminiAdapter_ThoughtSignatureCapturedOnToolCall(t *testing.T) {
	const sig = "AY89a18t+D98lADcFYKgjMgoHS7rOPAQUE=="
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"docs/safety-rings.md"}},"thoughtSignature":"` + sig + `"}]},"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	var toolCall *types.StreamEvent
	for i := range events {
		require.NotEqual(t, "error", events[i].Type, "unexpected error event: %v", events[i].Error)
		if events[i].Type == "tool_call" {
			toolCall = &events[i]
		}
	}
	require.NotNil(t, toolCall, "expected a tool_call event")
	require.Equal(t, sig, toolCall.ThoughtSignature)
}

// TestGeminiAdapter_ThoughtSignatureMissingIsEmpty confirms a pre-3.x
// response (no `thoughtSignature` on the part) leaves the StreamEvent's
// ThoughtSignature empty, so it round-trips via `omitempty`.
func TestGeminiAdapter_ThoughtSignatureMissingIsEmpty(t *testing.T) {
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"x"}}}]},"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	var toolCall *types.StreamEvent
	for i := range events {
		if events[i].Type == "tool_call" {
			toolCall = &events[i]
		}
	}
	require.NotNil(t, toolCall, "expected a tool_call event")
	require.Empty(t, toolCall.ThoughtSignature, "a response without thoughtSignature must leave it empty")
}

// TestGeminiAdapter_ThoughtSignatureCapturedViaDrainPath pins the drain
// branch of the SSE consumer: a tool call left open with willContinue=true
// is flushed when finishReason arrives, and the buffered thoughtSignature
// must travel with it onto the emitted StreamEvent.
func TestGeminiAdapter_ThoughtSignatureCapturedViaDrainPath(t *testing.T) {
	const sig = "DRAINABLE=="
	body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{"path":"x.go"},"willContinue":true},"thoughtSignature":"`+sig+`"}]}}]}`) +
		makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	events := collectEvents(t, ch)
	var calls []types.StreamEvent
	for _, ev := range events {
		require.NotEqual(t, "error", ev.Type, "unexpected error event: %v", ev.Error)
		if ev.Type == "tool_call" {
			calls = append(calls, ev)
		}
	}
	require.Len(t, calls, 1, "drain path must emit exactly one tool_call: %+v", calls)
	require.Equal(t, sig, calls[0].ThoughtSignature)
}

// TestGeminiAdapter_ThoughtSignatureLastNonEmptyWins pins the deliberate
// multi-chunk accumulation policy: across willContinue=true chunks the
// buffer retains the most recent NON-EMPTY thoughtSignature value; an
// absent field on a continuation chunk must not clobber a prior signature.
func TestGeminiAdapter_ThoughtSignatureLastNonEmptyWins(t *testing.T) {
	t.Run("later non-empty signature supersedes earlier", func(t *testing.T) {
		body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{"path":"a"},"willContinue":true},"thoughtSignature":"FIRST=="}]}}]}`) +
			makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{"path":"ab"},"willContinue":true}}]}}]}`) +
			makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"abc"}},"thoughtSignature":"LAST=="}]},"finishReason":"STOP"}]}`)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, body)
		}))
		defer srv.Close()

		adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
		ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.1-pro-preview"})
		require.NoError(t, err)

		events := collectEvents(t, ch)
		var toolCall *types.StreamEvent
		for i := range events {
			require.NotEqual(t, "error", events[i].Type, "unexpected error event: %v", events[i].Error)
			if events[i].Type == "tool_call" {
				toolCall = &events[i]
			}
		}
		require.NotNil(t, toolCall, "expected a tool_call event")
		require.Equal(t, "LAST==", toolCall.ThoughtSignature)
	})

	t.Run("absent field on later chunks does not clobber", func(t *testing.T) {
		body := makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{"path":"a"},"willContinue":true},"thoughtSignature":"ONLY=="}]}}]}`) +
			makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","partialArgs":{"path":"ab"},"willContinue":true}}]}}]}`) +
			makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"abc"}}}]},"finishReason":"STOP"}]}`)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, body)
		}))
		defer srv.Close()

		adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})
		ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.1-pro-preview"})
		require.NoError(t, err)

		events := collectEvents(t, ch)
		var toolCall *types.StreamEvent
		for i := range events {
			require.NotEqual(t, "error", events[i].Type, "unexpected error event: %v", events[i].Error)
			if events[i].Type == "tool_call" {
				toolCall = &events[i]
			}
		}
		require.NotNil(t, toolCall, "expected a tool_call event")
		require.Equal(t, "ONLY==", toolCall.ThoughtSignature, "later chunks without the field must not clobber the earlier value")
	})
}

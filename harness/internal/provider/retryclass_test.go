package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/types"
)

func TestDoWithRetry_Retries529ThenSucceeds(t *testing.T) {
	must := require.New(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(529)
			_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)
	req := newPostReq(t, srv.URL, `{"x":1}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	must.Equal(http.StatusOK, resp.StatusCode)
	must.EqualValues(2, attempts.Load())
	must.Equal(int64(1), retryOutcomeFromMetrics(t, reader)[retryOutcomeSucceeded])
}

func TestRetryOutcomeMetric_TaggedRequestScope(t *testing.T) {
	must := require.New(t)
	m, reader := newTestMetrics(t)
	recordOutcome(context.Background(), m, "anthropic", "claude", retryOutcomeSucceeded)
	must.Equal(int64(1), retryOutcomesByScope(t, reader)["request"])
}

func TestClassifyTransportError_RetryableKinds(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("connection refused"), false},
		{"idle stream", fmt.Errorf("read SSE stream: %w", fmt.Errorf("%w for 120s", errStreamIdle)), true},
		{"connection reset", fmt.Errorf("read SSE stream: %w", syscall.ECONNRESET), true},
		{"broken pipe", syscall.EPIPE, true},
		{"unexpected eof", fmt.Errorf("read SSE stream: %w", io.ErrUnexpectedEOF), true},
		{"eof", fmt.Errorf("execute request: %w", io.EOF), true},
		{"network timeout", timeoutError{}, true},
		{"context cancelled", fmt.Errorf("execute request: %w", context.Canceled), false},
		{"context deadline", fmt.Errorf("execute request: %w", context.DeadlineExceeded), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, types.IsRetryable(classifyTransportError(tc.err)))
		})
	}
}

// statusCase is a terminal pre-stream response and whether the adapter's
// error should be retryable at the turn level.
type statusCase struct {
	name   string
	status int
	body   string
	want   bool
}

func preStreamStatusCases() []statusCase {
	return []statusCase{
		{"401 unauthorized", http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`, false},
		{"400 bad request", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`, false},
		{"403 forbidden", http.StatusForbidden, `{"error":{"message":"forbidden","code":"misalignment_policy_violation"}}`, false},
		{"429 insufficient_quota", http.StatusTooManyRequests, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, false},
		{"429 rate limited", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, true},
		{"503 unavailable", http.StatusServiceUnavailable, `{"error":{"message":"unavailable"}}`, true},
		{"529 overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true},
		{"500 without body", http.StatusInternalServerError, "", true},
	}
}

// statusAdapters builds each DoWithRetry-backed adapter against url with a
// single-attempt retry policy and returns its pre-stream error.
func statusAdapters() map[string]func(url string) error {
	params := func(model string) types.StreamParams { return types.StreamParams{Model: model, MaxTokens: 1024} }
	return map[string]func(string) error{
		"anthropic": func(url string) error {
			a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
			a.baseURL = url
			_, err := a.Stream(context.Background(), params("claude-sonnet-4-6"))
			return err
		},
		"openai-compatible": func(url string) error {
			a := NewOpenAICompatibleAdapter(staticBearer("k"), url, OpenAIAuthConfig{}, RetryPolicy{})
			_, err := a.Stream(context.Background(), params("gpt-4o"))
			return err
		},
		"openai-responses": func(url string) error {
			a := NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
			_, err := a.Stream(context.Background(), params("gpt-4.1"))
			return err
		},
		"gemini": func(url string) error {
			a := newGeminiTestAdapter(url, &stubTokenSource{token: "t"})
			_, err := a.Stream(context.Background(), params("gemini-2.5-pro"))
			return err
		},
	}
}

func TestStreamingAdapters_ClassifyPreStreamStatus(t *testing.T) {
	for adapterName, stream := range statusAdapters() {
		for _, tc := range preStreamStatusCases() {
			t.Run(adapterName+"/"+tc.name, func(t *testing.T) {
				must := require.New(t)
				var hits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, tc.body)
				}))
				defer srv.Close()

				err := stream(srv.URL)
				must.Error(err)
				must.Equal(tc.want, types.IsRetryable(err))
				must.EqualValues(1, hits.Load(), "single-attempt policy should send one request")
			})
		}
	}
}

func TestAnthropicAdapter_Retries529ThenStreamsToCompletion(t *testing.T) {
	must := require.New(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(529)
			_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, joinLines(
			makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
			makeSSE("message_stop", `{}`),
		))
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
	a.baseURL = srv.URL
	a.RetryPolicy = RetryPolicy{MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, WallClockBudget: 5 * time.Second}
	ch, err := a.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
	must.NoError(err)
	events := collectEvents(t, ch)
	must.NotEmpty(events)
	must.Equal("message_complete", events[len(events)-1].Type, "events = %+v", events)
	must.EqualValues(2, hits.Load())
}

func TestAnthropicStreamError_Classification(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"overloaded error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true},
		{"api error", `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`, true},
		{"invalid request error", `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`, false},
		{"authentication error", `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, false},
		{"missing error type", `{"type":"error","error":{"message":"no type"}}`, false},
		{"non-json payload", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, types.IsRetryable(anthropicStreamError(tc.data)))
		})
	}
}

func TestSSE_OverloadedErrorEventIsRetryable(t *testing.T) {
	must := require.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		makeSSE("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	))
	must.NotEmpty(events)
	last := events[len(events)-1]
	must.Equal("error", last.Type, "last event = %+v", last)
	must.True(types.IsRetryable(last.Error), "last event = %+v, want a retryable error", last)
}

func TestAnthropicAdapter_ConnectionDroppedMidStreamIsRetryable(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlush(w, joinLines(
			makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
			makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		))
		conn, _, err := w.(http.Hijacker).Hijack()
		if !check.NoError(err, "hijack connection") {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
	a.baseURL = srv.URL
	ch, err := a.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
	must.NoError(err)
	events := collectEvents(t, ch)
	must.NotEmpty(events)
	last := events[len(events)-1]
	must.Equal("error", last.Type, "last event = %+v", last)
	must.True(types.IsRetryable(last.Error), "error = %v, want it marked retryable", last.Error)
}

func TestOpenAIResponsesStreamError_Classification(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "failed with server_error",
			body: makeResponsesEvent("response.failed", `{"response":{"status":"failed","error":{"code":"server_error","message":"The server had an error"}}}`),
			want: true,
		},
		{
			name: "error event with server_error",
			body: makeResponsesEvent("error", `{"code":"server_error","message":"The server had an error"}`),
			want: true,
		},
		{
			name: "failed with policy stop",
			body: makeResponsesEvent("response.failed", `{"response":{"status":"failed","error":{"code":"misalignment_policy_violation","message":"stopped"}}}`),
			want: false,
		},
		{
			name: "error event with quota code",
			body: makeResponsesEvent("error", `{"code":"insufficient_quota","message":"quota"}`),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			last := streamResponsesLastEvent(t, tc.body)
			must.Equal("error", last.Type, "last event = %+v", last)
			must.Equal(tc.want, types.IsRetryable(last.Error))
		})
	}
}

// retryOutcomesByScope sums provider_retry_outcomes data points by their
// scope attribute.
func retryOutcomesByScope(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm), "collect metrics")
	out := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.provider_retry_outcomes" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				scope, _ := dp.Attributes.Value("scope")
				out[scope.AsString()] += dp.Value
			}
		}
	}
	return out
}

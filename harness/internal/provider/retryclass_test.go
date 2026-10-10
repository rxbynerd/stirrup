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

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/types"
)

func TestDoWithRetry_529ThenSuccess(t *testing.T) {
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
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
	if got := retryOutcomeFromMetrics(t, reader)[retryOutcomeSucceeded]; got != 1 {
		t.Errorf("succeeded outcomes = %d, want 1", got)
	}
}

func TestRetryOutcomeMetric_TaggedRequestScope(t *testing.T) {
	m, reader := newTestMetrics(t)
	recordOutcome(context.Background(), m, "anthropic", "claude", retryOutcomeSucceeded)
	if got := retryOutcomesByScope(t, reader)["request"]; got != 1 {
		t.Errorf("request-scope outcomes = %d, want 1", got)
	}
}

func TestRetryableTransportErr(t *testing.T) {
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
		{"unexpected EOF", fmt.Errorf("read SSE stream: %w", io.ErrUnexpectedEOF), true},
		{"EOF", fmt.Errorf("execute request: %w", io.EOF), true},
		{"network timeout", timeoutError{}, true},
		{"context cancelled", fmt.Errorf("execute request: %w", context.Canceled), false},
		{"context deadline", fmt.Errorf("execute request: %w", context.DeadlineExceeded), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := types.IsRetryable(classifyTransportError(tc.err)); got != tc.want {
				t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
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
				var hits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, tc.body)
				}))
				defer srv.Close()

				err := stream(srv.URL)
				if err == nil {
					t.Fatalf("Stream() succeeded, want a status %d error", tc.status)
				}
				if got := types.IsRetryable(err); got != tc.want {
					t.Errorf("IsRetryable(%v) = %v, want %v", err, got, tc.want)
				}
				if n := hits.Load(); n != 1 {
					t.Errorf("server saw %d requests, want 1 under a single-attempt policy", n)
				}
			})
		}
	}
}

func TestAnthropicAdapter_529ThenSuccessThroughDoWithRetry(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)
	if len(events) == 0 || events[len(events)-1].Type != "message_complete" {
		t.Fatalf("events = %+v, want a trailing message_complete", events)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("server saw %d requests, want 2", n)
	}
}

func TestAnthropicStreamError_Classification(t *testing.T) {
	cases := []struct {
		data string
		want bool
	}{
		{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true},
		{`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`, true},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`, false},
		{`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, false},
		{`{"type":"error","error":{"message":"no type"}}`, false},
		{`not json`, false},
	}
	for _, tc := range cases {
		if got := types.IsRetryable(anthropicStreamError(tc.data)); got != tc.want {
			t.Errorf("anthropicStreamError(%s) retryable = %v, want %v", tc.data, got, tc.want)
		}
	}
}

func TestSSE_OverloadedErrorEventIsRetryable(t *testing.T) {
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		makeSSE("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	))
	last := events[len(events)-1]
	if last.Type != "error" || !types.IsRetryable(last.Error) {
		t.Fatalf("last event = %+v, want a retryable error", last)
	}
}

func TestAnthropicAdapter_ConnectionDroppedMidStreamIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlush(w, joinLines(
			makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
			makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		))
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
	a.baseURL = srv.URL
	ch, err := a.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("last event = %+v, want an error", last)
	}
	if !types.IsRetryable(last.Error) {
		t.Errorf("error = %v, want it marked retryable", last.Error)
	}
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
			last := streamResponsesLastEvent(t, tc.body)
			if last.Type != "error" {
				t.Fatalf("last event = %+v, want an error", last)
			}
			if got := types.IsRetryable(last.Error); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", last.Error, got, tc.want)
			}
		})
	}
}

// retryOutcomesByScope sums provider_retry_outcomes data points by their
// scope attribute.
func retryOutcomesByScope(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
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

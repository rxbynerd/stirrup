package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// Error bodies below follow the documented OpenAI shapes (error-codes guide,
// misalignment-monitoring guide, ResponseErrorEvent and ResponseError in the
// OpenAPI spec). They are documented, not probed against the live API.

// streamResponsesSSE serves body as a 200 SSE response and returns the
// adapter's terminal event.
func streamResponsesLastEvent(t *testing.T, body string) types.StreamEvent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	return events[len(events)-1]
}

func TestOpenAIResponsesAdapter_FailedEventIncludesErrorCode(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			name: "message and code",
			data: `{"type":"response.failed","response":{"status":"failed","error":{"code":"misalignment_policy_violation","message":"This conversation was stopped by misalignment monitoring."}}}`,
			want: "openai responses API: This conversation was stopped by misalignment monitoring. (code: misalignment_policy_violation)",
		},
		{
			name: "code only",
			data: `{"response":{"status":"failed","error":{"code":"server_error","message":""}}}`,
			want: "openai responses API: response failed (code: server_error)",
		},
		{
			name: "message only",
			data: `{"response":{"status":"failed","error":{"message":"server overloaded"}}}`,
			want: "openai responses API: server overloaded",
		},
		{
			name: "null error",
			data: `{"response":{"status":"failed","error":null}}`,
			want: "openai responses API: response failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			last := streamResponsesLastEvent(t, makeResponsesEvent("response.failed", tc.data))
			if last.Type != "error" || last.Error == nil {
				t.Fatalf("last event = %+v, want an error event", last)
			}
			if got := last.Error.Error(); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpenAIResponsesAdapter_ErrorEventIncludesCode(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			name: "mid-stream misalignment stop",
			data: `{"type":"error","code":"misalignment_policy_violation","message":"Blocked by misalignment monitoring.","param":null,"sequence_number":7}`,
			want: "openai responses API stream error: Blocked by misalignment monitoring. (code: misalignment_policy_violation)",
		},
		{
			name: "null code",
			data: `{"type":"error","code":null,"message":"upstream timeout","param":null,"sequence_number":1}`,
			want: "openai responses API stream error: upstream timeout",
		},
		{
			name: "code only",
			data: `{"type":"error","code":"server_error","message":""}`,
			want: "openai responses API stream error: (code: server_error)",
		},
		{
			name: "numeric code",
			data: `{"type":"error","code":500,"message":"gateway failure"}`,
			want: "openai responses API stream error: gateway failure (code: 500)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Join([]string{
				makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`),
				makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"partial"}`),
				makeResponsesEvent("error", tc.data),
			}, "")
			last := streamResponsesLastEvent(t, body)
			if last.Type != "error" || last.Error == nil {
				t.Fatalf("last event = %+v, want an error event", last)
			}
			if got := last.Error.Error(); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpenAIResponsesAdapter_HTTP403MisalignmentIncludesCodeAndIsNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"message":"This request was blocked by misalignment monitoring.","type":"invalid_request_error","param":null,"code":"misalignment_policy_violation"}}`)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.RetryPolicy = fastRetryPolicy()

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6-astra", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	want := "openai responses API returned status 403: This request was blocked by misalignment monitoring. (code: misalignment_policy_violation)"
	if got := err.Error(); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("server attempts = %d, want 1: a misalignment stop must not be retried", got)
	}
}

func TestOpenAIResponsesAdapter_HTTPErrorWithoutCodeKeepsMessageOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Invalid input","type":"invalid_request_error","code":null}}`)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if got, want := err.Error(), "openai responses API returned status 400: Invalid input"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestOpenAIResponsesAdapter_HTTPErrorBodies(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "non-JSON gateway page",
			status: http.StatusBadGateway,
			body:   `<html><body>502 Bad Gateway</body></html>`,
			want:   "openai responses API returned status 502",
		},
		{
			name:   "code only",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"invalid_prompt"}}`,
			want:   "openai responses API returned status 400: (code: invalid_prompt)",
		},
		{
			name:   "numeric code",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"Bad request from gateway","code":400}}`,
			want:   "openai responses API returned status 400: Bad request from gateway (code: 400)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
			adapter.RetryPolicy = RetryPolicy{MaxAttempts: 1}
			_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 1024})
			if err == nil {
				t.Fatalf("expected an error for status %d", tc.status)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOpenAIResponsesAdapter_QuotaExhausted429SurfacesCode drives the
// quota classification through the Responses adapter: one attempt, and the
// caller's error message still decodes the body the classifier read.
func TestOpenAIResponsesAdapter_QuotaExhausted429SurfacesCode(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Organization spend limit reached.","type":"insufficient_quota","param":null,"code":"organization_spend_limit_exceeded"}}`)
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	adapter.RetryPolicy = fastRetryPolicy()

	_, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-6", MaxTokens: 1024})
	if err == nil {
		t.Fatal("expected an error for a quota 429")
	}
	want := "openai responses API returned status 429: Organization spend limit reached. (code: organization_spend_limit_exceeded)"
	if got := err.Error(); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("server attempts = %d, want 1", got)
	}
}

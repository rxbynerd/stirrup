package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// Quota bodies follow the documented OpenAI error-codes guide; they are
// documented, not probed against the live API.

func TestDoWithRetry_QuotaExhausted429IsNotRetried(t *testing.T) {
	bodies := map[string]string{
		"insufficient_quota code":            `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`,
		"insufficient_quota type, null code": `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":null}}`,
		"credit_balance_exhausted":           `{"error":{"message":"No prepaid credits remain.","type":"insufficient_quota","code":"credit_balance_exhausted"}}`,
		"organization_spend_limit_exceeded":  `{"error":{"message":"Organization spend limit reached.","type":"insufficient_quota","code":"organization_spend_limit_exceeded"}}`,
		"project_spend_limit_exceeded":       `{"error":{"message":"Project spend limit reached.","type":"insufficient_quota","code":"project_spend_limit_exceeded"}}`,
		"organization_usage_limit_exceeded":  `{"error":{"message":"Organization usage limit reached.","type":"insufficient_quota","code":"organization_usage_limit_exceeded"}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			m, reader := newTestMetrics(t)
			resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second},
				newPostReq(t, srv.URL, `{}`), testOpts(defaultTestPolicy(), m))
			if err != nil {
				t.Fatalf("DoWithRetry: %v", err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })

			if resp.StatusCode != http.StatusTooManyRequests {
				t.Errorf("status = %d, want 429", resp.StatusCode)
			}
			if got := atomic.LoadInt32(&attempts); got != 1 {
				t.Errorf("attempts = %d, want 1", got)
			}
			if outcomes := retryOutcomeFromMetrics(t, reader); outcomes[retryOutcomeNonRetryable] != 1 {
				t.Errorf("non_retryable counter = %d, want 1 (all: %+v)", outcomes[retryOutcomeNonRetryable], outcomes)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(got) != body {
				t.Errorf("body after classification = %q, want the full original %q", got, body)
			}
		})
	}
}

func TestDoWithRetry_Other429sStillRetried(t *testing.T) {
	bodies := map[string]string{
		"slow_down":           `{"error":{"message":"Slow down.","type":"rate_limit_error","code":"slow_down"}}`,
		"rate_limit_exceeded": `{"error":{"message":"Rate limit reached.","type":"requests","code":"rate_limit_exceeded"}}`,
		"anthropic shape":     `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`,
		"numeric code":        `{"error":{"code":429,"message":"Resource exhausted.","status":"RESOURCE_EXHAUSTED"}}`,
		"not json":            `too many requests`,
		"empty":               ``,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&attempts, 1) == 1 {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, body)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second},
				newPostReq(t, srv.URL, `{}`), testOpts(defaultTestPolicy(), nil))
			if err != nil {
				t.Fatalf("DoWithRetry: %v", err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 after a retry", resp.StatusCode)
			}
			if got := atomic.LoadInt32(&attempts); got != 2 {
				t.Errorf("attempts = %d, want 2", got)
			}
		})
	}
}

func TestDoWithRetry_403IsNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Blocked.","type":"invalid_request_error","code":"misalignment_policy_violation"}}`)
	}))
	defer srv.Close()

	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second},
		newPostReq(t, srv.URL, `{}`), testOpts(defaultTestPolicy(), nil))
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestQuotaExhausted_LeavesNon429BodyUntouched(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"error":{"code":"insufficient_quota"}}`))
	resp := &http.Response{StatusCode: http.StatusInternalServerError, Body: body}
	if quotaExhausted(resp) {
		t.Error("quotaExhausted = true for a 500, want false")
	}
	if resp.Body != body {
		t.Error("quotaExhausted replaced the body of a non-429 response")
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

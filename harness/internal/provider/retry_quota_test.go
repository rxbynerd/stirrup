package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Quota bodies follow the documented OpenAI error-codes guide; they are
// documented, not probed against the live API.

func TestDoWithRetry_QuotaExhausted429IsNotRetried(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"insufficient_quota code", `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, "insufficient_quota"},
		{"insufficient_quota type, null code", `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":null}}`, "insufficient_quota"},
		{"insufficient_quota type, numeric code", `{"error":{"code":429,"type":"insufficient_quota"}}`, "insufficient_quota"},
		{"credit_balance_exhausted", `{"error":{"message":"No prepaid credits remain.","type":"insufficient_quota","code":"credit_balance_exhausted"}}`, "credit_balance_exhausted"},
		{"organization_spend_limit_exceeded", `{"error":{"message":"Organization spend limit reached.","type":"insufficient_quota","code":"organization_spend_limit_exceeded"}}`, "organization_spend_limit_exceeded"},
		{"project_spend_limit_exceeded", `{"error":{"message":"Project spend limit reached.","type":"insufficient_quota","code":"project_spend_limit_exceeded"}}`, "project_spend_limit_exceeded"},
		{"organization_usage_limit_exceeded", `{"error":{"message":"Organization usage limit reached.","type":"insufficient_quota","code":"organization_usage_limit_exceeded"}}`, "organization_usage_limit_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			m, reader := newTestMetrics(t)
			var logBuf bytes.Buffer
			opts := testOpts(defaultTestPolicy(), m)
			opts.Logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
			resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second},
				newPostReq(t, srv.URL, `{}`), opts)
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
			outcomes := retryOutcomeFromMetrics(t, reader)
			if outcomes[retryOutcomeQuotaExhausted] != 1 || outcomes[retryOutcomeNonRetryable] != 0 {
				t.Errorf("retry outcomes = %+v, want quota_exhausted=1 and no non_retryable", outcomes)
			}
			var warn map[string]any
			if err := json.Unmarshal(logBuf.Bytes(), &warn); err != nil {
				t.Fatalf("decode quota log line %q: %v", logBuf.String(), err)
			}
			if warn["msg"] != "provider_quota_exhausted" || warn["level"] != "WARN" || warn["code"] != tc.wantCode {
				t.Errorf("quota log = %v, want a WARN provider_quota_exhausted with code %q", warn, tc.wantCode)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(got) != tc.body {
				t.Errorf("body after classification = %q, want the full original %q", got, tc.body)
			}
		})
	}
}

// TestDoWithRetry_Other429sStillRetried pins that a 429 the quota check
// does not match is retried, and that the body of the returned response is
// intact after the peek.
func TestDoWithRetry_Other429sStillRetried(t *testing.T) {
	quotaCodeBeyondPeek := `{"error":{"message":"` + strings.Repeat("x", maxQuotaBodyPeek) + `","code":"insufficient_quota"}}`
	bodies := map[string]string{
		"slow_down":                  `{"error":{"message":"Slow down.","type":"rate_limit_error","code":"slow_down"}}`,
		"rate_limit_exceeded":        `{"error":{"message":"Rate limit reached.","type":"requests","code":"rate_limit_exceeded"}}`,
		"anthropic rate_limit_error": `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`,
		"gemini RESOURCE_EXHAUSTED":  `{"error":{"code":429,"message":"Resource exhausted.","status":"RESOURCE_EXHAUSTED"}}`,
		"html gateway page":          `<html><head><title>429 Too Many Requests</title></head><body>nginx</body></html>`,
		"truncated json":             `{"error":{"code":"insufficient_qu`,
		"not json":                   `too many requests`,
		"empty":                      ``,
		"quota code beyond the peek": quotaCodeBeyondPeek,
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

			policy := defaultTestPolicy()
			policy.MaxAttempts = 2
			m, reader := newTestMetrics(t)
			resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second},
				newPostReq(t, srv.URL, `{}`), testOpts(policy, m))
			if err != nil {
				t.Fatalf("DoWithRetry: %v", err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })

			if got := atomic.LoadInt32(&attempts); got != 2 {
				t.Errorf("attempts = %d, want 2", got)
			}
			if outcomes := retryOutcomeFromMetrics(t, reader); outcomes[retryOutcomeExhausted] != 1 {
				t.Errorf("retry outcomes = %+v, want exhausted=1", outcomes)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(got) != body {
				t.Errorf("body = %q (%d bytes), want the full original (%d bytes)", truncateForTest(string(got)), len(got), len(body))
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

func TestClassifyRetryable_ConsumedShouldRetryWinsOverQuota(t *testing.T) {
	for _, verdict := range []bool{true, false} {
		body := io.NopCloser(strings.NewReader(`{"error":{"code":"insufficient_quota"}}`))
		resp := &http.Response{StatusCode: http.StatusTooManyRequests, Body: body}
		shouldRetry := func(*http.Response) (bool, bool) { return verdict, true }

		retryable, quotaCode := classifyRetryable(resp, shouldRetry)
		if retryable != verdict || quotaCode != "" {
			t.Errorf("classifyRetryable = (%v, %q), want (%v, \"\")", retryable, quotaCode, verdict)
		}
		if resp.Body != body {
			t.Errorf("verdict %v: classifyRetryable replaced the body despite a consumed ShouldRetry", verdict)
		}
	}
}

func TestPeekQuotaExhausted_LeavesNon429BodyUntouched(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`{"error":{"code":"insufficient_quota"}}`))
	resp := &http.Response{StatusCode: http.StatusInternalServerError, Body: body}
	if _, exhausted := peekQuotaExhausted(resp); exhausted {
		t.Error("peekQuotaExhausted = true for a 500, want false")
	}
	if resp.Body != body {
		t.Error("peekQuotaExhausted replaced the body of a non-429 response")
	}
}

// oneShotErrReader returns data, then err once, then io.EOF, so a caller
// that re-reads after the error would otherwise see a clean truncation.
type oneShotErrReader struct {
	data    string
	err     error
	errSent bool
}

func (r *oneShotErrReader) Read(p []byte) (int, error) {
	if r.data != "" {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.errSent {
		r.errSent = true
		return 0, r.err
	}
	return 0, io.EOF
}

func TestPeekQuotaExhausted_ReadErrorReachesCaller(t *testing.T) {
	errBoom := errors.New("connection reset mid-body")
	partial := `{"error":{"code":"insuff`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(&oneShotErrReader{data: partial, err: errBoom}),
	}

	if code, exhausted := peekQuotaExhausted(resp); exhausted {
		t.Errorf("peekQuotaExhausted = (%q, true) for a partial body, want false", code)
	}
	got, err := io.ReadAll(resp.Body)
	if string(got) != partial {
		t.Errorf("body = %q, want the peeked bytes %q", got, partial)
	}
	if !errors.Is(err, errBoom) {
		t.Errorf("read error = %v, want %v", err, errBoom)
	}
}

func truncateForTest(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

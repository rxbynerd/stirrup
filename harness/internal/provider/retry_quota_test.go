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

	"github.com/stretchr/testify/require"
)

// Quota bodies follow the documented OpenAI error-codes guide; they are
// documented, not probed against the live API.

func TestDoWithRetry_QuotaExhausted429IsNotRetried(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"insufficient quota code", `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, "insufficient_quota"},
		{"insufficient quota type with null code", `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":null}}`, "insufficient_quota"},
		{"insufficient quota type with numeric code", `{"error":{"code":429,"type":"insufficient_quota"}}`, "insufficient_quota"},
		{"credit balance exhausted", `{"error":{"message":"No prepaid credits remain.","type":"insufficient_quota","code":"credit_balance_exhausted"}}`, "credit_balance_exhausted"},
		{"organization spend limit exceeded", `{"error":{"message":"Organization spend limit reached.","type":"insufficient_quota","code":"organization_spend_limit_exceeded"}}`, "organization_spend_limit_exceeded"},
		{"project spend limit exceeded", `{"error":{"message":"Project spend limit reached.","type":"insufficient_quota","code":"project_spend_limit_exceeded"}}`, "project_spend_limit_exceeded"},
		{"organization usage limit exceeded", `{"error":{"message":"Organization usage limit reached.","type":"insufficient_quota","code":"organization_usage_limit_exceeded"}}`, "organization_usage_limit_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
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
			must.NoError(err)
			t.Cleanup(func() { _ = resp.Body.Close() })

			must.Equal(http.StatusTooManyRequests, resp.StatusCode)
			must.Equal(int32(1), atomic.LoadInt32(&attempts))

			outcomes := retryOutcomeFromMetrics(t, reader)
			must.Equal(int64(1), outcomes[retryOutcomeQuotaExhausted], "retry outcomes: %+v", outcomes)
			must.Zero(outcomes[retryOutcomeNonRetryable], "retry outcomes: %+v", outcomes)

			var warn map[string]any
			must.NoError(json.Unmarshal(logBuf.Bytes(), &warn), "quota log line: %q", logBuf.String())
			must.Equal("provider_quota_exhausted", warn["msg"], "quota log: %v", warn)
			must.Equal("WARN", warn["level"], "quota log: %v", warn)
			must.Equal(tc.wantCode, warn["code"], "quota log: %v", warn)

			got, err := io.ReadAll(resp.Body)
			must.NoError(err)
			must.Equal(tc.body, string(got))
		})
	}
}

// TestDoWithRetry_NonQuota429sAreRetried pins that a 429 the quota check
// does not match is retried, and that the body of the returned response is
// intact after the peek.
func TestDoWithRetry_NonQuota429sAreRetried(t *testing.T) {
	quotaCodeBeyondPeek := `{"error":{"message":"` + strings.Repeat("x", maxQuotaBodyPeek) + `","code":"insufficient_quota"}}`
	bodies := []struct {
		name string
		body string
	}{
		{"slow down", `{"error":{"message":"Slow down.","type":"rate_limit_error","code":"slow_down"}}`},
		{"rate limit exceeded", `{"error":{"message":"Rate limit reached.","type":"requests","code":"rate_limit_exceeded"}}`},
		{"anthropic rate limit error", `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`},
		{"gemini resource exhausted", `{"error":{"code":429,"message":"Resource exhausted.","status":"RESOURCE_EXHAUSTED"}}`},
		{"html gateway page", `<html><head><title>429 Too Many Requests</title></head><body>nginx</body></html>`},
		{"truncated json", `{"error":{"code":"insufficient_qu`},
		{"not json", `too many requests`},
		{"empty", ``},
		{"quota code beyond the peek", quotaCodeBeyondPeek},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			policy := defaultTestPolicy()
			policy.MaxAttempts = 2
			m, reader := newTestMetrics(t)
			resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second},
				newPostReq(t, srv.URL, `{}`), testOpts(policy, m))
			must.NoError(err)
			t.Cleanup(func() { _ = resp.Body.Close() })

			must.Equal(int32(2), atomic.LoadInt32(&attempts))

			outcomes := retryOutcomeFromMetrics(t, reader)
			must.Equal(int64(1), outcomes[retryOutcomeExhausted], "retry outcomes: %+v", outcomes)

			got, err := io.ReadAll(resp.Body)
			must.NoError(err)
			must.Equal(tc.body, string(got))
		})
	}
}

func TestDoWithRetry_403IsNotRetried(t *testing.T) {
	must := require.New(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Blocked.","type":"invalid_request_error","code":"misalignment_policy_violation"}}`)
	}))
	defer srv.Close()

	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second},
		newPostReq(t, srv.URL, `{}`), testOpts(defaultTestPolicy(), nil))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(http.StatusForbidden, resp.StatusCode)
	must.Equal(int32(1), atomic.LoadInt32(&attempts))
}

func TestClassifyRetryable_ConsumedShouldRetryWinsOverQuota(t *testing.T) {
	cases := []struct {
		name    string
		verdict bool
	}{
		{"retry verdict wins over quota", true},
		{"no-retry verdict wins over quota", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			body := io.NopCloser(strings.NewReader(`{"error":{"code":"insufficient_quota"}}`))
			resp := &http.Response{StatusCode: http.StatusTooManyRequests, Body: body}
			shouldRetry := func(*http.Response) (bool, bool) { return tc.verdict, true }

			retryable, quotaCode := classifyRetryable(resp, shouldRetry)
			must.Equal(tc.verdict, retryable)
			must.Empty(quotaCode)
			must.True(resp.Body == body, "classifyRetryable replaced the body despite a consumed ShouldRetry") //nolint:testifylint // identity comparison; Equal would compare by value
		})
	}
}

func TestPeekQuotaExhausted_LeavesNon429BodyUntouched(t *testing.T) {
	must := require.New(t)
	body := io.NopCloser(strings.NewReader(`{"error":{"code":"insufficient_quota"}}`))
	resp := &http.Response{StatusCode: http.StatusInternalServerError, Body: body}

	_, exhausted := peekQuotaExhausted(resp)
	must.False(exhausted, "a 500 response must not report quota exhaustion")
	must.True(resp.Body == body, "peekQuotaExhausted replaced the body of a non-429 response") //nolint:testifylint // identity comparison; Equal would compare by value
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
	must := require.New(t)
	errBoom := errors.New("connection reset mid-body")
	partial := `{"error":{"code":"insuff`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(&oneShotErrReader{data: partial, err: errBoom}),
	}

	code, exhausted := peekQuotaExhausted(resp)
	must.False(exhausted, "partial body reported quota code %q", code)

	got, err := io.ReadAll(resp.Body)
	must.Equal(partial, string(got), "peeked bytes must be replayed to the caller")
	must.ErrorIs(err, errBoom)
}

package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func TestParseRetryAfter_HeaderPrecedenceAndBounds(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		headers    http.Header
		want       time.Duration
		wantSource string
	}{
		{
			name:       "retry-after-ms integer",
			headers:    http.Header{"Retry-After-Ms": []string{"1500"}},
			want:       1500 * time.Millisecond,
			wantSource: delaySourceRetryAfterMs,
		},
		{
			name:       "retry-after integer seconds",
			headers:    http.Header{"Retry-After": []string{"2"}},
			want:       2 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			name:       "retry-after http-date",
			headers:    http.Header{"Retry-After": []string{now.Add(5 * time.Second).Format(http.TimeFormat)}},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			name: "retry-after-ms wins over retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"500"},
				"Retry-After":    []string{"10"},
			},
			want:       500 * time.Millisecond,
			wantSource: delaySourceRetryAfterMs,
		},
		{
			name:       "negative retry-after returns zero",
			headers:    http.Header{"Retry-After": []string{"-1"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "garbage retry-after returns zero",
			headers:    http.Header{"Retry-After": []string{"not-a-number"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "negative retry-after-ms returns zero",
			headers:    http.Header{"Retry-After-Ms": []string{"-100"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "no headers returns zero",
			headers:    http.Header{},
			want:       0,
			wantSource: "",
		},
		{
			name: "zero retry-after-ms falls through to retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"0"},
				"Retry-After":    []string{"5"},
			},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			name: "negative retry-after-ms falls through to retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"-1"},
				"Retry-After":    []string{"5"},
			},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			name:       "retry-after-ms at ceiling returns 60s",
			headers:    http.Header{"Retry-After-Ms": []string{"60000"}},
			want:       60 * time.Second,
			wantSource: delaySourceRetryAfterMs,
		},
		{
			name:       "retry-after-ms above ceiling falls through",
			headers:    http.Header{"Retry-After-Ms": []string{"60001"}},
			want:       0,
			wantSource: "",
		},
		{
			name: "retry-after-ms above ceiling falls through to retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"60001"},
				"Retry-After":    []string{"5"},
			},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			// 9223372036954776 ms * 1e6 (ns/ms) would wrap int64; the
			// ceiling rejects the value first.
			name:       "retry-after-ms near int64 overflow returns zero",
			headers:    http.Header{"Retry-After-Ms": []string{"9223372036954776"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "retry-after seconds above ceiling returns zero",
			headers:    http.Header{"Retry-After": []string{"3600"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "retry-after seconds at ceiling returns 60s",
			headers:    http.Header{"Retry-After": []string{"60"}},
			want:       60 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got, source := parseRetryAfter(tt.headers, now)
			must.Equal(tt.want, got, "duration")
			must.Equal(tt.wantSource, source, "source")
		})
	}
}

func TestRetryableStatus_ClassifiesStatusCodes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{"200 ok", 200, false},
		{"201 created", 201, false},
		{"301 moved permanently", 301, false},
		{"400 bad request", 400, false},
		{"401 unauthorized", 401, false},
		{"403 forbidden", 403, false},
		{"404 not found", 404, false},
		{"408 request timeout", 408, true},
		{"409 conflict", 409, true},
		{"410 gone", 410, false},
		{"422 unprocessable entity", 422, false},
		{"429 too many requests", 429, true},
		{"500 internal server error", 500, true},
		{"501 not implemented", 501, false},
		{"502 bad gateway", 502, true},
		{"503 service unavailable", 503, true},
		{"504 gateway timeout", 504, true},
		{"505 http version not supported", 505, false},
		{"529 overloaded", 529, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, retryableStatus(tt.status))
		})
	}
}

func TestBackoffDelay_SamplesStayBelowCap(t *testing.T) {
	must := require.New(t)
	policy := RetryPolicy{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     5 * time.Second,
	}
	prng := rand.New(rand.NewPCG(1, 2))

	for n := 0; n < 8; n++ {
		expectedCap := policy.InitialDelay << n
		if expectedCap > policy.MaxDelay || expectedCap <= 0 {
			expectedCap = policy.MaxDelay
		}
		for i := 0; i < 1000; i++ {
			got := backoffDelay(n, policy, prng)
			must.GreaterOrEqual(got, time.Duration(0), "attempt %d sample %d", n, i)
			must.Less(got, expectedCap, "attempt %d sample %d", n, i)
		}
	}
}

// newTestMetrics returns a Metrics instance backed by a ManualReader so
// assertions can inspect the recorded counter increments.
func newTestMetrics(t *testing.T) (*observability.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	must.NoError(err)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return m, reader
}

// retryOutcomeFromMetrics extracts the recorded provider_retry_outcomes
// counter data points keyed by outcome.
func retryOutcomeFromMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))
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
				outcome, _ := dp.Attributes.Value("provider.retry.outcome")
				out[outcome.AsString()] += dp.Value
			}
		}
	}
	return out
}

func newPostReq(t *testing.T, url, body string) *http.Request {
	t.Helper()
	must := require.New(t)
	bodyBytes := []byte(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	must.NoError(err)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}
	return req
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func defaultTestPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    5 * time.Millisecond,
		MaxDelay:        50 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}
}

// testOpts builds a RetryOptions struct from the most common test
// inputs. Keeps the per-test call sites short.
func testOpts(policy RetryPolicy, m *observability.Metrics) RetryOptions {
	return RetryOptions{
		Policy:       policy,
		Logger:       discardLogger(),
		Metrics:      m,
		ProviderType: "openai",
		Model:        "gpt-test",
	}
}

// withRecordingSpan returns a context carrying an OTel span backed by an
// in-memory exporter so tests can assert on span events.
func withRecordingSpan(t *testing.T) (context.Context, *tracetest.InMemoryExporter, oteltrace.Span) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	tracer := tp.Tracer("retry-test")
	ctx, span := tracer.Start(context.Background(), "test-span")
	return ctx, exporter, span
}

func TestDoWithRetry_RetriesOn429UntilSuccess(t *testing.T) {
	must := require.New(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)
	ctx, exporter, span := withRecordingSpan(t)

	req := newPostReq(t, srv.URL, `{"x":1}`)
	resp, err := DoWithRetry(ctx, &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(http.StatusOK, resp.StatusCode)
	must.EqualValues(2, atomic.LoadInt32(&attempts))

	span.End()
	events := collectSpanEvents(exporter, "provider_retry_attempt")
	must.Len(events, 1, "provider_retry_attempt events")

	outcomes := retryOutcomeFromMetrics(t, reader)
	must.Equal(int64(1), outcomes[retryOutcomeSucceeded], "succeeded counter")
}

func TestDoWithRetry_RetryAfterSecondsHonoured(t *testing.T) {
	must := require.New(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := defaultTestPolicy()
	policy.MaxDelay = 2 * time.Second

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	elapsed := time.Since(start)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.GreaterOrEqual(elapsed, 800*time.Millisecond, "Retry-After: 1 should wait about 1s")
	must.LessOrEqual(elapsed, 1200*time.Millisecond, "Retry-After: 1 should wait about 1s")
}

func TestDoWithRetry_RetryAfterMsHonoured(t *testing.T) {
	must := require.New(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After-Ms", "250")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := defaultTestPolicy()
	policy.MaxDelay = 1 * time.Second

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	elapsed := time.Since(start)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.GreaterOrEqual(elapsed, 150*time.Millisecond, "Retry-After-Ms: 250 should wait about 250ms")
	must.LessOrEqual(elapsed, 450*time.Millisecond, "Retry-After-Ms: 250 should wait about 250ms")
}

func TestDoWithRetry_ReturnsLastResponseWhenAttemptsExhausted(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        5 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	check.Equal(http.StatusTooManyRequests, resp.StatusCode)
	check.EqualValues(3, atomic.LoadInt32(&attempts))

	outcomes := retryOutcomeFromMetrics(t, reader)
	check.Equal(int64(1), outcomes[retryOutcomeExhausted], "exhausted counter")
}

func TestDoWithRetry_StopsWhenWallClockBudgetExhausted(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force a large server-side hint so the first sleep would
		// overshoot the budget.
		w.Header().Set("Retry-After-Ms", "5000")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     5,
		InitialDelay:    10 * time.Millisecond,
		MaxDelay:        10 * time.Second,
		WallClockBudget: 50 * time.Millisecond,
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	check.Equal(http.StatusTooManyRequests, resp.StatusCode)

	outcomes := retryOutcomeFromMetrics(t, reader)
	check.Equal(int64(1), outcomes[retryOutcomeBudgetExhausted],
		"budget_exhausted counter (all outcomes: %+v)", outcomes)
}

func TestDoWithRetry_DoesNotRetryNonRetryableStatus(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	check.Equal(http.StatusBadRequest, resp.StatusCode)
	check.EqualValues(1, atomic.LoadInt32(&attempts))

	outcomes := retryOutcomeFromMetrics(t, reader)
	check.Equal(int64(1), outcomes[retryOutcomeNonRetryable], "non_retryable counter")
}

func TestDoWithRetry_ContextCancelDuringSleep(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "5000")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     5,
		InitialDelay:    10 * time.Millisecond,
		MaxDelay:        10 * time.Second,
		WallClockBudget: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(ctx, &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	elapsed := time.Since(start)

	must.ErrorIs(err, context.Canceled)
	must.Nil(resp)
	check.LessOrEqual(elapsed, 200*time.Millisecond, "cancel should return promptly")

	outcomes := retryOutcomeFromMetrics(t, reader)
	check.Equal(int64(1), outcomes[retryOutcomeContextDone], "context_done counter")
}

func TestDoWithRetry_BodyRewoundOnRetry(t *testing.T) {
	must := require.New(t)
	var (
		mu     sync.Mutex
		bodies []string
		count  int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(buf))
		mu.Unlock()
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	bodyContent := `{"prompt":"hello world"}`
	req := newPostReq(t, srv.URL, bodyContent)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	mu.Lock()
	defer mu.Unlock()
	must.Len(bodies, 2)
	must.Equal([]string{bodyContent, bodyContent}, bodies, "every attempt should resend the same body")
}

func TestDoWithRetry_PanicsWhenGetBodyMissing(t *testing.T) {
	must := require.New(t)
	defer func() {
		r := recover()
		must.NotNil(r, "expected a panic")
		must.Contains(fmt.Sprint(r), "DoWithRetry: req.GetBody must be set")
	}()

	req, err := http.NewRequest(http.MethodPost, "http://example.invalid", strings.NewReader("body"))
	must.NoError(err)
	// http.NewRequest with strings.NewReader sets GetBody automatically;
	// clear it to exercise the contract check.
	req.GetBody = nil

	_, _ = DoWithRetry(context.Background(), &http.Client{}, req,
		testOpts(defaultTestPolicy(), nil))
}

// collectSpanEvents extracts events with the given name across all spans
// captured by the in-memory exporter.
func collectSpanEvents(exporter *tracetest.InMemoryExporter, name string) []sdktrace.Event {
	var out []sdktrace.Event
	for _, sp := range exporter.GetSpans() {
		for _, ev := range sp.Events {
			if ev.Name == name {
				out = append(out, ev)
			}
		}
	}
	return out
}

// timeoutOpError synthesises a net.OpError whose underlying error
// reports Timeout()==true. The retry helper classifies transport
// errors via errors.As(*net.Error) + Timeout(); this is the
// shortest path to a value that satisfies that contract.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestTransientErr_ClassifiesByErrorAndAttempt(t *testing.T) {
	t.Run("nil error is not transient", func(t *testing.T) {
		must := require.New(t)
		must.False(transientErr(nil, 0))
	})
	t.Run("net.OpError with timeout is transient", func(t *testing.T) {
		must := require.New(t)
		opErr := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
		must.True(transientErr(opErr, 0), "first attempt")
		// Timeouts stay transient on later attempts.
		must.True(transientErr(opErr, 1), "second attempt")
	})
	t.Run("io.EOF on first attempt is transient", func(t *testing.T) {
		must := require.New(t)
		must.True(transientErr(io.EOF, 0))
	})
	t.Run("io.EOF on second attempt is not transient", func(t *testing.T) {
		must := require.New(t)
		// A repeated EOF points at a server-side condition the next
		// attempt would also hit, not a stale keep-alive.
		must.False(transientErr(io.EOF, 1))
	})
	t.Run("plain error is not transient", func(t *testing.T) {
		must := require.New(t)
		must.False(transientErr(errors.New("connection reset"), 0))
	})
}

// hijackAndCloseHandler returns an http.HandlerFunc that hijacks the
// connection and closes it immediately, simulating a stale-keepalive
// EOF on the client. If alwaysClose is false, the handler closes only
// on the first request and answers 200 OK on every subsequent one.
func hijackAndCloseHandler(t *testing.T, alwaysClose bool, count *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(count, 1)
		if alwaysClose || n == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter does not support Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func TestDoWithRetry_TransientNetworkError(t *testing.T) {
	must := require.New(t)
	var count int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, false, &count))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	// Connection re-use lets the first request observe the
	// close-after-hijack, and the second attempt opens a fresh connection.
	client := &http.Client{Timeout: 5 * time.Second}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), client, req, testOpts(defaultTestPolicy(), m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(http.StatusOK, resp.StatusCode)
	must.Equal(int32(2), atomic.LoadInt32(&count), "server hit count")

	outcomes := retryOutcomeFromMetrics(t, reader)
	must.Equal(int64(1), outcomes[retryOutcomeSucceeded], "outcomes: %+v", outcomes)
}

func TestDoWithRetry_TransientNetworkError_RecordsSpanAttr(t *testing.T) {
	must := require.New(t)
	// Uses the hijack-and-close server from TestDoWithRetry_TransientNetworkError
	// with a recording span, so the span event for the retry is inspected.
	var count int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, false, &count))
	defer srv.Close()

	m, _ := newTestMetrics(t)
	ctx, exporter, span := withRecordingSpan(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(ctx, &http.Client{Timeout: 5 * time.Second}, req, testOpts(defaultTestPolicy(), m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	span.End()
	events := collectSpanEvents(exporter, "provider_retry_attempt")
	must.Len(events, 1, "provider_retry_attempt events")

	// The exact error text varies by platform ("EOF", "connection reset
	// by peer"), so only its presence and the absence of the URL are checked.
	var errValues []string
	for _, kv := range events[0].Attributes {
		if string(kv.Key) == "error" {
			errValues = append(errValues, kv.Value.AsString())
		}
	}
	must.NotEmpty(errValues, "provider_retry_attempt event error attribute")
	for _, v := range errValues {
		must.NotContains(v, srv.URL, "span error attribute leaks URL")
	}
}

func TestDoWithRetry_PersistentEOF_GivesUpAfterSecondAttempt(t *testing.T) {
	must := require.New(t)
	var count int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, true, &count))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	must.Error(err, "persistent EOF must surface as an error")

	// Exactly two attempts: the first sees EOF and is retried; the second
	// sees EOF with attempt>0, so transientErr is false and the error
	// is reported as non-retryable.
	must.Equal(int32(2), atomic.LoadInt32(&count), "server hit count")

	outcomes := retryOutcomeFromMetrics(t, reader)
	must.Equal(int64(1), outcomes[retryOutcomeNonRetryable], "outcomes: %+v", outcomes)
}

func TestDoWithRetry_TricklingRetryBodyDrainIsTimeBounded(t *testing.T) {
	must := require.New(t)
	const drainTimeout = 200 * time.Millisecond
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		for range 100 { // 100 x 50 ms = 5 s, far past drainTimeout
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	client := newStreamingHTTPClient()
	defer client.CloseIdleConnections()
	opts := testOpts(RetryPolicy{MaxAttempts: 2, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond}, nil)
	opts.drainTimeout = drainTimeout

	start := time.Now()
	resp, err := DoWithRetry(context.Background(), client, newPostReq(t, srv.URL, `{}`), opts)
	elapsed := time.Since(start)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(http.StatusOK, resp.StatusCode)
	must.Equal(int32(2), hits.Load(), "server request count")
	must.GreaterOrEqual(elapsed, drainTimeout, "retry returned before the drain ran")
	must.LessOrEqual(elapsed, 2*time.Second, "drain should be cut off shortly after %v", drainTimeout)
}

func TestRetryPolicyFromConfig_ConvertsFieldsAndHandlesNil(t *testing.T) {
	t.Run("nil cfg returns zero policy", func(t *testing.T) {
		must := require.New(t)
		must.Equal(RetryPolicy{}, RetryPolicyFromConfig(nil))
	})
	t.Run("populated cfg converts ms fields to durations", func(t *testing.T) {
		must := require.New(t)
		cfg := &types.ProviderRetryConfig{
			MaxAttempts:       3,
			InitialDelayMs:    200,
			MaxDelayMs:        5000,
			WallClockBudgetMs: 30000,
		}
		want := RetryPolicy{
			MaxAttempts:     3,
			InitialDelay:    200 * time.Millisecond,
			MaxDelay:        5 * time.Second,
			WallClockBudget: 30 * time.Second,
		}
		must.Equal(want, RetryPolicyFromConfig(cfg))
	})
}

// TestRetryPolicyFromConfig_ZeroInitialDelay asserts a caller that
// bypasses ValidateRunConfig with InitialDelayMs=0 still gets the safe
// canonical default rather than a zero InitialDelay.
func TestRetryPolicyFromConfig_ZeroInitialDelay(t *testing.T) {
	must := require.New(t)
	cfg := &types.ProviderRetryConfig{
		MaxAttempts:       3,
		InitialDelayMs:    0,
		MaxDelayMs:        5000,
		WallClockBudgetMs: 30000,
	}
	want := RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    500 * time.Millisecond,
		MaxDelay:        5 * time.Second,
		WallClockBudget: 30 * time.Second,
	}
	must.Equal(want, RetryPolicyFromConfig(cfg), "zero InitialDelayMs falls back to 500ms")
}

func TestBackoffDelay_ZeroInitialDelay(t *testing.T) {
	must := require.New(t)
	policy := RetryPolicy{
		InitialDelay: 0,
		MaxDelay:     1 * time.Second,
	}
	prng := rand.New(rand.NewPCG(1, 2))
	for i := range 100 {
		must.Zero(backoffDelay(i%5, policy, prng), "attempt %d", i%5)
	}
}

// A zero MaxDelay caps every backoff at zero.
func TestBackoffDelay_ZeroMaxDelay(t *testing.T) {
	must := require.New(t)
	policy := RetryPolicy{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     0,
	}
	prng := rand.New(rand.NewPCG(1, 2))
	for i := range 100 {
		must.Zero(backoffDelay(i%5, policy, prng), "attempt %d", i%5)
	}
}

func TestDoWithRetry_RetryAfterMsCapped(t *testing.T) {
	must := require.New(t)
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			// Within parseRetryAfter's 60 s ceiling, but well above
			// the test policy's 100 ms MaxDelay — exercises the cap.
			w.Header().Set("Retry-After-Ms", "5000")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        100 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	elapsed := time.Since(start)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	// Uncapped, the 5 s Retry-After-Ms would be honoured.
	must.LessOrEqual(elapsed, time.Second, "retry should be capped near MaxDelay (100ms)")
}

func TestDoWithRetry_RetryAfterSecondsCapped(t *testing.T) {
	must := require.New(t)
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			// parseRetryAfter caps Retry-After at 60 s (its own
			// ceiling). Use 30 s so the value reaches the dispatch
			// switch's MaxDelay cap unmodified by the header parser.
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        100 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	elapsed := time.Since(start)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	// Uncapped, the 30 s Retry-After would be honoured.
	must.LessOrEqual(elapsed, time.Second, "retry should be capped near MaxDelay (100ms)")
}

func TestDoWithRetry_MaxAttemptsZeroNormalisedToOne(t *testing.T) {
	must := require.New(t)
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	// MaxAttempts == 0 must be normalised to one attempt by the
	// helper. The for-loop bound is the only protection against a
	// zero-attempts misconfiguration causing the loop to never run.
	policy := RetryPolicy{MaxAttempts: 0, InitialDelay: 1 * time.Millisecond}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(int32(1), atomic.LoadInt32(&count), "server hits")
}

func TestDoWithRetry_NilMetrics(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Pass nil metrics; recordOutcome's nil guard must prevent panic.
	req := newPostReq(t, srv.URL, `{}`)
	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Logger:       discardLogger(),
		Metrics:      nil,
		ProviderType: "openai",
		Model:        "gpt-test",
	}
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(http.StatusOK, resp.StatusCode)
}

// flakyGetBody is a counter-backed GetBody implementation that returns
// the body successfully on first call (for http.NewRequest's initial
// read) and fails on subsequent calls. Used to drive the
// retryOutcomeRewindFailed path.
type flakyGetBody struct {
	body    []byte
	calls   int32
	failAt  int32
	failErr error
}

func (f *flakyGetBody) get() (io.ReadCloser, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n >= f.failAt {
		return nil, f.failErr
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func TestDoWithRetry_RewindFailureStopsWithoutExhaustion(t *testing.T) {
	must := require.New(t)
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	bodyBytes := []byte(`{"x":1}`)
	injected := errors.New("rewind failed: source exhausted")
	// failAt=1 makes the first GetBody call fail. That call rewinds the
	// body for attempt 1; attempt 0 reads req.Body directly.
	flaky := &flakyGetBody{body: bodyBytes, failAt: 1, failErr: injected}

	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(bodyBytes))
	must.NoError(err)
	req.GetBody = flaky.get

	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	must.ErrorIs(err, injected)
	must.Nil(resp, "rewind failure returns no response")

	// Exactly one server hit: the first attempt succeeded in sending,
	// the response was 429, then GetBody failed before attempt 2 could
	// be dispatched.
	must.Equal(int32(1), atomic.LoadInt32(&count), "server hits")

	outcomes := retryOutcomeFromMetrics(t, reader)
	must.Equal(int64(1), outcomes[retryOutcomeRewindFailed], "outcomes: %+v", outcomes)
	// A rewind failure must not also be recorded as exhaustion.
	must.Equal(int64(0), outcomes[retryOutcomeExhausted], "outcomes: %+v", outcomes)
}

func TestDoWithRetry_ShouldRetry_ConsumesAndRetries(t *testing.T) {
	must := require.New(t)
	// ShouldRetry returns (retryable=true, consumed=true) on the
	// first attempt's 200 response. Exercises the "consumed=true"
	// branch overriding the default heuristic (which would treat
	// 200 as success).
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			w.Header().Set("x-force-retry", "1")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Logger:       discardLogger(),
		Metrics:      m,
		ProviderType: "anthropic",
		Model:        "claude-test",
		ShouldRetry: func(resp *http.Response) (bool, bool) {
			if resp.Header.Get("x-force-retry") == "1" {
				return true, true
			}
			return false, false
		},
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(int32(2), atomic.LoadInt32(&count), "server hits")
}

func TestDoWithRetry_ShouldRetry_FallsThroughOnNotConsumed(t *testing.T) {
	must := require.New(t)
	// ShouldRetry returns (false, false=not consumed) for every
	// response. The default retryableStatus heuristic should apply
	// — first 429 retries, second 200 succeeds.
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	var shouldRetryCalls int32
	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Logger:       discardLogger(),
		Metrics:      m,
		ProviderType: "anthropic",
		Model:        "claude-test",
		ShouldRetry: func(resp *http.Response) (bool, bool) {
			atomic.AddInt32(&shouldRetryCalls, 1)
			return false, false
		},
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(int32(2), atomic.LoadInt32(&count), "server hits")
	// ShouldRetry is consulted on the 429 response and falls through to
	// the default heuristic.
	must.Positive(atomic.LoadInt32(&shouldRetryCalls), "ShouldRetry was never consulted")
}

// TestDoWithRetry_NilLoggerFallback asserts the nil-logger fallback
// emits a provider_retry warn record and wraps slog.Default() in a
// ScrubHandler, so a scrubbable sentinel does not land raw.
func TestDoWithRetry_NilLoggerFallback(t *testing.T) {
	must := require.New(t)
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&count, 1); n == 1 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	// Swap the process-global slog default for the duration of the
	// test so we can inspect what reaches the fallback handler.
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Logger left zero so DoWithRetry's nil-fallback branch fires.
	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Metrics:      m,
		ProviderType: "openai",
		Model:        "gpt-test-sk-ant-test-cafebabe1234567890",
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	must.Equal(http.StatusOK, resp.StatusCode)
	logOutput := buf.String()
	must.Contains(logOutput, "provider_retry", "nil-logger fallback emitted no provider_retry record")
	// The Model field carries a scrubbable Anthropic key. Without the
	// ScrubHandler wrap it reaches the warn record verbatim.
	must.NotContains(logOutput, "sk-ant-test-cafebabe1234567890", "nil-logger fallback leaked a scrubbable sentinel")
	must.Contains(logOutput, "[REDACTED]", "nil-logger fallback should scrub the sentinel")
}

func TestDoWithRetry_TransportError_URLNotLogged(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	m, _ := newTestMetrics(t)

	bodyBytes := []byte(`{}`)

	// Use a short-timeout dialer so the test does not block on
	// connection retry timeouts.
	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext,
		},
	}

	policy := RetryPolicy{
		MaxAttempts:     2,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        5 * time.Millisecond,
		WallClockBudget: 2 * time.Second,
	}

	opts := RetryOptions{
		Policy:       policy,
		Logger:       logger,
		Metrics:      m,
		ProviderType: "openai",
		Model:        "gpt-test",
	}

	// The server closes the first connection, so the helper retries and
	// logs the failed attempt. The request URL carries the secrets that
	// must not reach the log.
	var hits int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, false, &hits))
	defer srv.Close()

	secretURL := srv.URL + "/path?api_key=topsecret&token=alsosecret"
	req, err := http.NewRequest(http.MethodPost, secretURL, bytes.NewReader(bodyBytes))
	must.NoError(err)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}

	buf.Reset()
	resp, err := DoWithRetry(context.Background(), client, req, opts)
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	logOutput := buf.String()
	check.NotContains(logOutput, "topsecret", "log output contains the api_key query value")
	check.NotContains(logOutput, "alsosecret", "log output contains the token query value")
	// The *url.Error wrapping the transport error includes the request
	// URL, so the server host must be absent from the log as well.
	host := strings.TrimPrefix(srv.URL, "http://")
	check.NotContains(logOutput, host, "log output contains the server URL host")
}

// scrubbableTransport is a stub http.RoundTripper that fails on the
// first call with a timeout error embedding a scrubbable pattern, so
// transientErr classifies it as retryable and the helper logs a retry
// attempt rather than surfacing the error immediately.
type scrubbableTransport struct {
	mu       sync.Mutex
	calls    int
	innerErr error
	delegate http.RoundTripper
}

func (t *scrubbableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.calls == 1 {
		return nil, t.innerErr
	}
	return t.delegate.RoundTrip(req)
}

// scrubTimeoutErr satisfies net.Error.Timeout()==true so transientErr
// classifies it as retryable. Embedding a scrub-pattern in the message
// is the load-bearing piece — it lets the span attribute assertion
// verify that the scrubber runs on the value the OTel exporter sees.
type scrubTimeoutErr struct{ msg string }

func (e *scrubTimeoutErr) Error() string   { return e.msg }
func (e *scrubTimeoutErr) Timeout() bool   { return true }
func (e *scrubTimeoutErr) Temporary() bool { return true }

// TestDoWithRetry_SpanErrorAttributeIsScrubbed asserts the OTel span's
// `error` attribute is run through security.Scrub before export,
// mirroring the existing slog scrubbing at the same code site.
func TestDoWithRetry_SpanErrorAttributeIsScrubbed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Use a known scrubbable pattern (Anthropic API key prefix). The
	// scrubber replaces it with "[REDACTED]" — asserting on the
	// presence of the literal token in the span attribute would tell
	// us the scrubber did not run.
	secret := "sk-ant-test-abcdef1234567890"
	transport := &scrubbableTransport{
		innerErr: &scrubTimeoutErr{msg: "dial tcp: " + secret + " timed out"},
		delegate: http.DefaultTransport,
	}

	// Need a real server for attempt 2 (helper retries on the timeout).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	m, _ := newTestMetrics(t)
	ctx, exporter, span := withRecordingSpan(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(ctx, client, req, testOpts(defaultTestPolicy(), m))
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	span.End()
	events := collectSpanEvents(exporter, "provider_retry_attempt")
	must.Len(events, 1, "provider_retry_attempt events")

	var got string
	var found bool
	for _, kv := range events[0].Attributes {
		if string(kv.Key) == "error" {
			found = true
			got = kv.Value.AsString()
		}
	}
	must.True(found, "provider_retry_attempt event has no error attribute")

	check.NotContains(got, secret, "span error attribute leaks the scrubbable token")
	check.Contains(got, "[REDACTED]", "span error attribute should contain the scrub marker")
	// The non-secret message prefix must survive scrubbing.
	check.True(strings.HasPrefix(got, "dial tcp: "), "span error attribute lost its message prefix, got %q", got)
}
